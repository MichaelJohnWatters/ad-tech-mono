package main

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/adserving"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ara"
	arapg "github.com/MichaelJohnWatters/ad-tech-mono/pkg/ara/postgres"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/privacy"
)

// araUUIDRe validates the advid so a garbage value can't reach the ::uuid cast
// (and to keep a clean 400 rather than a 500).
var araUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// araMaxDestLen bounds the destination string.
const araMaxDestLen = 512

// ARA observability. Registered on the tracker's metrics registry in main.
var (
	araSourcesRegistered = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "adtech", Subsystem: "ara", Name: "sources_registered_total",
		Help: "Privacy Sandbox ARA attribution sources registered via /v1/t/ara/src.",
	})
	araReportsIngested = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "adtech", Subsystem: "ara", Name: "reports_ingested_total",
		Help: "ARA reports resolved to an account and persisted, by type.",
	}, []string{"type"})
	araReportsDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "adtech", Subsystem: "ara", Name: "reports_dropped_total",
		Help: "ARA reports accepted but dropped (malformed / no registered source), by type and reason.",
	}, []string{"type", "reason"})
	araReportsQuarantined = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "adtech", Subsystem: "ara", Name: "reports_quarantined_total",
		Help: "Aggregatable ARA reports accepted into the platform quarantine (never tenant-attributed).",
	})
	araTriggersServed = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "adtech", Subsystem: "ara", Name: "triggers_served_total",
		Help: "Attribution-Reporting-Register-Trigger headers served via the browser-side /v1/t/ara/trigger beacon (consented).",
	})
)

// araCollectors returns the ARA metrics for registration on the tracker registry.
func araCollectors() []prometheus.Collector {
	return []prometheus.Collector{araSourcesRegistered, araReportsIngested, araReportsDropped, araReportsQuarantined, araTriggersServed}
}

// Privacy Sandbox ARA endpoints on the tracker (the single ARA reporting origin
// for this platform). See docs/attribution-phase4-ara.md and pkg/ara.
//
// REPORTING-ONLY overlay: nothing here bills or touches the exact conversions
// stream. Registration (source + trigger headers) is gated by tracker.ara_enabled
// AND consent (privacy.Evaluate().Personalise) — an unconsented request registers
// nothing. The report-ingest endpoints are the unauthenticated callbacks the
// browser POSTs to; they resolve the owning advertiser account through the store's
// platform hatch and persist the raw report to the SEPARATE ara_reports table.
//
// MOCK BOUNDARY (only a real Privacy-Sandbox browser does these): the source↔
// trigger match, the k-anonymity noise, the multi-day delay, and the aggregation
// service decrypt. We build correct headers and accept/persist reports; we never
// fake the match or decrypt payloads.
type araDeps struct {
	store   *arapg.Store
	enabled func() bool
	// sigKeys / sigStrict enforce the same signature gate as the other tracker
	// beacons on source registration, so advid can't be forged: the ad server
	// bakes a signed /v1/t/ara/src; an unsigned request is rejected in strict mode
	// (tracker.signature_validation) exactly like /v1/t/conv.
	sigKeys       func(advid string) []string
	sigStrict     func() bool
	expValidation func() bool
	log           *slog.Logger
	maxBody       int64
}

// araConsented reports whether the request carries personalisation consent — the
// same gate the deterministic identity path uses. NOTE: the ARA beacons carry no
// TCF/GPP consent string, so in practice this honors the browser's Sec-GPC header
// (or ?gpc=1) / explicit opt-out params; with no signals it defaults to consented.
// The binding control is the SOURCE gate (the ad server bakes a source only on a
// consented serve), so an unconsented user has no source for a trigger to match.
func araConsented(r *http.Request) bool {
	q := r.URL.Query()
	return privacy.Evaluate(privacy.SignalsFromQuery(q.Get, r.Header.Get("Sec-GPC"))).Personalise
}

// registerSource: GET /v1/t/ara/src?advid=&dest=&cid= — records an attribution
// source and returns the Attribution-Reporting-Register-Source header a browser
// reads. In production the ad server bakes this (signed, attributionsrc) into the
// served creative; the browser fetches it attribution-eligible.
func (d araDeps) registerSource(w http.ResponseWriter, r *http.Request) {
	if !d.enabled() || d.store == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	q := r.URL.Query()
	advid, dest := q.Get("advid"), q.Get("dest")
	if !araUUIDRe.MatchString(advid) || dest == "" || len(dest) > araMaxDestLen {
		http.Error(w, `{"error":"advid (uuid) and dest required"}`, http.StatusBadRequest)
		return
	}
	// Anti-spoofing: the source beacon is signed like every other tracker beacon,
	// so advid is bound to a key an attacker can't produce. In strict mode an
	// invalid signature is rejected (in dev warn mode it's lenient, matching the
	// rest of the tracker) — this is what stops an unauthenticated cross-tenant
	// write of ara_sources for an arbitrary advertiser account.
	if !adserving.ValidateSignatureAny(r.URL.Path, q, d.sigKeys(advid)) && d.sigStrict() {
		http.Error(w, `{"error":"invalid signature"}`, http.StatusForbidden)
		return
	}
	// Replay/expiry: reject a stale signed URL (exp=<unix-ts> in the past), same
	// as every other tracker beacon — a captured source beacon can't be replayed
	// past its window. URLs without exp pass (isExpired returns false).
	if d.expValidation() && isExpired(q, time.Now()) {
		http.Error(w, `{"error":"url expired"}`, http.StatusGone)
		return
	}
	if !araConsented(r) { // no personalisation consent → register nothing
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Canonicalise dest to scheme + eTLD+1 (the shape a real browser reports back),
	// so even a validly signed but hand-crafted beacon can't record an arbitrary
	// destination, and the stored value can't drift from what a browser sends (F5).
	// The ad server produces the same value via ara.NormalizeDestination.
	dest, ok := ara.NormalizeDestination(dest)
	if !ok {
		http.Error(w, `{"error":"dest not a valid site"}`, http.StatusBadRequest)
		return
	}

	sid := mintSourceID()
	sidStr := strconv.FormatUint(sid, 10)
	src := ara.Source{
		SourceEventID:   sid,
		Destination:     dest,
		Expiry:          ara.DefaultSourceExpiry,
		AggregationKeys: map[string]string{"campaignCounts": "0x1"},
	}
	hdr, err := src.MarshalHeader()
	if err != nil {
		http.Error(w, `{"error":"bad source"}`, http.StatusBadRequest)
		return
	}
	if err := d.store.RecordSource(r.Context(), ara.SourceRegistration{
		SourceEventID: sidStr, AccountID: advid, Destination: dest,
		CampaignID: q.Get("cid"), ExpiresAt: time.Now().Add(ara.DefaultSourceExpiry),
	}); err != nil {
		d.log.Error("ara: record source failed", "advid", advid, "error", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	araSourcesRegistered.Inc()
	w.Header().Set(ara.HeaderRegisterSource, hdr)
	w.WriteHeader(http.StatusNoContent)
}

// setTriggerHeader adds the Attribution-Reporting-Register-Trigger response
// header for a conversion when ARA is on and the request is consented; a no-op
// otherwise. Shared by the browser-side /v1/t/ara/trigger beacon (registerTrigger,
// the live path a real browser acts on) and the S2S /v1/t/conv handler (where it
// is inert — nothing reads it on a server-to-server postback, but it costs nothing
// and keeps the header available if a signed browser conv ever fires it).
func (d araDeps) setTriggerHeader(w http.ResponseWriter, r *http.Request, convType string, revenue float64) {
	if !d.enabled() || !araConsented(r) {
		return
	}
	// A stable, low-entropy trigger_data from the conversion type (ARA masks it to
	// the source's configured bit width anyway). Value carries a coarse revenue
	// bucket into the aggregatable histogram.
	t := ara.Trigger{
		EventTriggerData: []ara.EventTriggerData{{TriggerData: triggerDataForType(convType)}},
		AggregatableTriggerData: []ara.AggregatableTriggerData{
			{KeyPiece: "0x400", SourceKeys: []string{"campaignCounts"}},
		},
		AggregatableValues: map[string]int{"campaignCounts": revenueBucket(revenue)},
	}
	if hdr, err := t.MarshalHeader(); err == nil {
		w.Header().Set(ara.HeaderRegisterTrigger, hdr)
		araTriggersServed.Inc()
	}
}

// registerTrigger: GET /v1/t/ara/trigger?type=&rev=&cur= — the browser-side ARA
// trigger beacon. The advertiser embeds an attributionsrc pointing here on their
// conversion page; a supporting browser fetches it attribution-eligible, reads the
// Attribution-Reporting-Register-Trigger header, and matches it to a source it
// registered earlier (same destination + reporting origin).
//
// Reporting-only and deliberately UNSIGNED: it writes nothing server-side and
// never bills (CPA billing stays on the signed, S2S /v1/t/conv), so a third-party
// page firing it just gets a header the browser only acts on when it already holds
// a matching source. Consent + tracker.ara_enabled gate the header (in
// setTriggerHeader); with either off this is a bare 204.
func (d araDeps) registerTrigger(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	revenue, _ := strconv.ParseFloat(q.Get("rev"), 64)
	d.setTriggerHeader(w, r, q.Get("type"), revenue)
	w.WriteHeader(http.StatusNoContent)
}

// ingestEvent / ingestAggregate are the well-known report callbacks the browser
// POSTs to. They resolve the owning account and persist the raw report. Reports
// we can't tie to a registered source are ACCEPTED (200) but dropped — returning
// a non-2xx would make the browser retry a report we can never place.
func (d araDeps) ingestEvent(w http.ResponseWriter, r *http.Request) {
	d.ingest(w, r, ara.ReportEvent)
}

func (d araDeps) ingestAggregate(w http.ResponseWriter, r *http.Request) {
	d.ingest(w, r, ara.ReportAggregate)
}

func (d araDeps) ingest(w http.ResponseWriter, r *http.Request, typ ara.ReportType) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if d.store == nil {
		http.Error(w, `{"error":"ara unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, d.maxBody))
	if err != nil {
		http.Error(w, `{"error":"read"}`, http.StatusBadRequest)
		return
	}

	// AGGREGATABLE reports carry no unguessable source id — the only thing tying
	// one to an advertiser is the PUBLIC attribution_destination, which anyone can
	// name. Attributing on that basis is a cross-tenant write (F1), and we can't
	// verify/decrypt the payloads without the aggregation service (the mock
	// boundary) anyway. So we never resolve them to a tenant: parse, drop into the
	// platform-global quarantine (never shown in an advertiser overlay), done.
	if typ == ara.ReportAggregate {
		rep, perr := ara.ParseAggregatableReport(body)
		if perr != nil {
			d.log.Warn("ara: malformed aggregatable report (accepted, dropped)", "error", perr)
			araReportsDropped.WithLabelValues(string(typ), "malformed").Inc()
			w.WriteHeader(http.StatusOK)
			return
		}
		if _, err := d.store.QuarantineAggregatable(r.Context(), rep.AttributionDestination, rep.ReportID, body); err != nil {
			d.log.Error("ara: quarantine aggregatable report failed", "error", err)
			http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
			return
		}
		araReportsQuarantined.Inc()
		d.log.Info("ara: aggregatable report quarantined (not tenant-attributed)", "claimed_dest", rep.AttributionDestination)
		w.WriteHeader(http.StatusOK)
		return
	}

	// EVENT report: resolve ONLY by the unguessable source_event_id we minted — no
	// destination fallback, so a report can't be steered into another tenant.
	rep, perr := ara.ParseEventReport(body)
	if perr != nil {
		d.log.Warn("ara: malformed event report (accepted, dropped)", "error", perr)
		araReportsDropped.WithLabelValues(string(typ), "malformed").Inc()
		w.WriteHeader(http.StatusOK)
		return
	}
	acct, err := d.store.ResolveAccount(r.Context(), rep.SourceEventID)
	if errors.Is(err, arapg.ErrNoAccount) {
		d.log.Warn("ara: event report for an unregistered source (dropped)", "type", typ)
		araReportsDropped.WithLabelValues(string(typ), "unresolved").Inc()
		w.WriteHeader(http.StatusOK)
		return
	}
	if err != nil {
		d.log.Error("ara: resolve account failed", "error", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}

	inserted, err := d.store.SaveReport(r.Context(), acct, ara.StoredReport{
		ReportType: typ, AttributionDestination: rep.AttributionDestination, SourceEventID: rep.SourceEventID,
		TriggerData: rep.TriggerData, ReportID: rep.ReportID, Body: body,
	})
	if err != nil {
		d.log.Error("ara: save report failed", "error", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	araReportsIngested.WithLabelValues(string(typ)).Inc()
	d.log.Info("ara: event report ingested", "account", acct, "new", inserted)
	w.WriteHeader(http.StatusOK)
}

// araQuarantineRetention bounds how long a (staff-inspectable, unauthenticated)
// aggregatable-quarantine row lives before the purge loop drops it.
const araQuarantineRetention = 7 * 24 * time.Hour

// araPurgeLoop periodically drops expired ara_sources rows and old quarantine rows
// (housekeeping so neither the registration log nor the quarantine — fed by an
// unauthenticated ingest — can grow unbounded). Both deletes are batched in the
// store so a backlog can't lock a table. Stops when ctx is cancelled.
func araPurgeLoop(ctx context.Context, store *arapg.Store, log *slog.Logger) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := store.DeleteExpiredSources(context.Background()); err != nil {
				log.Warn("ara: purge expired sources failed", "error", err)
			} else if n > 0 {
				log.Info("ara: purged expired sources", "count", n)
			}
			if n, err := store.DeleteOldQuarantine(context.Background(), araQuarantineRetention); err != nil {
				log.Warn("ara: purge quarantine failed", "error", err)
			} else if n > 0 {
				log.Info("ara: purged old quarantined reports", "count", n)
			}
		}
	}
}

// mintSourceID returns a random 64-bit source id (the entropy the event-level
// report round-trips).
func mintSourceID() uint64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint64(b[:])
}

// triggerDataForType maps a conversion type to a small trigger_data value.
func triggerDataForType(convType string) uint64 {
	switch convType {
	case "purchase":
		return 1
	case "signup", "lead":
		return 2
	case "add_to_cart":
		return 3
	default:
		return 0
	}
}

// revenueBucket coarsens revenue into an aggregatable histogram value (a small
// non-zero integer; ARA aggregatable values are bounded per report).
func revenueBucket(revenue float64) int {
	switch {
	case revenue <= 0:
		return 1
	case revenue < 10:
		return 8
	case revenue < 100:
		return 64
	default:
		return 512
	}
}
