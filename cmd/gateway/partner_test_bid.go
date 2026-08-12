package main

// partner_test_bid.go — partner self-serve OpenRTB conformance tools (PLAN
// Phase 11 #112 slice 3):
//
//	POST /v1/api/partner/validate  — body = the partner's sample BidResponse JSON;
//	                                 checked against a golden request → {valid, findings}
//	POST /v1/api/partner/test-bid  — send a golden BidRequest to the partner's
//	                                 REGISTERED sandbox endpoint, validate the
//	                                 response → {valid, latency_ms, http_status, findings}
//
// Both are partner-account-only (partner:self). The conformance rules live in
// pkg/openrtb (reusable). A no-bid is valid; an unreachable/malformed endpoint is
// reported gracefully, never a 500.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/openrtb"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/partner"
)

// blockedProbeIP reports whether the test-bid probe must NOT connect to ip —
// loopback / private / link-local (incl. cloud metadata 169.254.169.254) /
// unspecified. Partner bid endpoints are public, so blocking these closes the
// SSRF surface (the gateway must never be steered into the cluster or a metadata
// service by a registered endpoint value).
func blockedProbeIP(ip net.IP) bool {
	return ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// safeProbeClient is the http.Client for the test-bid probe. It does NOT follow
// redirects (a 3xx could hop past the scheme/IP check to an internal target) and
// blocks connections to non-public IPs at CONNECT time via the dialer Control
// hook — which sees the RESOLVED address, so it's DNS-rebinding-safe.
func safeProbeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if blockedProbeIP(net.ParseIP(host)) {
				return fmt.Errorf("refusing to connect to non-public address %s", host)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// test-bid is an authenticated outbound probe; a light per-account cooldown stops
// a partner turning the gateway into a request amplifier against its endpoint.
var (
	testBidMu   sync.Mutex
	testBidLast = map[string]time.Time{}
)

const testBidMinInterval = 2 * time.Second

// testBidResult is the response of both endpoints.
type testBidResult struct {
	Valid      bool              `json:"valid"`
	Findings   []openrtb.Finding `json:"findings"`
	LatencyMs  int64             `json:"latency_ms,omitempty"`
	HTTPStatus int               `json:"http_status,omitempty"`
	Note       string            `json:"note,omitempty"`
}

// partnerValidateHandler validates a BidResponse the partner pastes in — no
// outbound call, fully deterministic.
func partnerValidateHandler(log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, ok := partnerGate(w, r, "partner:self"); !ok {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
		if err != nil {
			http.Error(w, `{"error":"read"}`, http.StatusBadRequest)
			return
		}
		golden := openrtb.GoldenBidRequest("validate-probe")
		var resp openrtb.BidResponse
		if jerr := json.Unmarshal(body, &resp); jerr != nil {
			// A body that won't parse as a BidResponse is itself a conformance error.
			_ = json.NewEncoder(w).Encode(testBidResult{
				Valid:    false,
				Findings: []openrtb.Finding{{Severity: openrtb.SevError, Field: "", Message: "response is not valid JSON / not an OpenRTB BidResponse: " + jerr.Error()}},
			})
			return
		}
		fs := openrtb.ValidateBidResponse(&golden, &resp)
		_ = json.NewEncoder(w).Encode(testBidResult{Valid: !openrtb.HasErrors(fs), Findings: fs})
	}
}

// partnerTestBidHandler sends a golden request to the partner's registered
// endpoint and validates what comes back.
func partnerTestBidHandler(store partner.Store, log *slog.Logger) http.HandlerFunc {
	client := safeProbeClient(5 * time.Second)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		accountID, ok := partnerGate(w, r, "partner:self")
		if !ok {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		// Per-account cooldown (amplification guard).
		testBidMu.Lock()
		if now := time.Now(); now.Sub(testBidLast[accountID]) < testBidMinInterval {
			testBidMu.Unlock()
			http.Error(w, `{"error":"slow down — one test-bid every couple of seconds"}`, http.StatusTooManyRequests)
			return
		} else {
			testBidLast[accountID] = now
		}
		testBidMu.Unlock()
		if store == nil {
			http.Error(w, `{"error":"partners unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		p, err := store.GetByAccount(r.Context(), accountID)
		if err != nil {
			http.Error(w, `{"error":"no partner record for this account"}`, http.StatusNotFound)
			return
		}
		if p.EndpointBid == "" {
			http.Error(w, `{"error":"no bid endpoint registered — ask your account manager to set one"}`, http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(p.EndpointBid, "http://") && !strings.HasPrefix(p.EndpointBid, "https://") {
			http.Error(w, `{"error":"test-bid can only probe http(s):// endpoints (grpc:// is validated during certification)"}`, http.StatusBadRequest)
			return
		}

		golden := openrtb.GoldenBidRequest("testbid-" + accountID)
		reqBody, _ := json.Marshal(golden)

		// Bound the probe independently of the request context.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.EndpointBid, bytes.NewReader(reqBody))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("X-OpenRTB-Version", "2.6")

		start := time.Now()
		httpResp, err := client.Do(httpReq)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			_ = json.NewEncoder(w).Encode(testBidResult{
				Valid:     false,
				LatencyMs: latency,
				Findings:  []openrtb.Finding{{Severity: openrtb.SevError, Field: "endpoint", Message: "could not reach the endpoint: " + err.Error()}},
				Note:      "the probe couldn't complete a request to your registered bid endpoint",
			})
			return
		}
		defer httpResp.Body.Close()
		respBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 512<<10))

		res := testBidResult{LatencyMs: latency, HTTPStatus: httpResp.StatusCode}
		switch {
		case httpResp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(respBody)) == 0:
			// 204 / empty body = a valid no-bid.
			res.Valid = true
			res.Note = "no-bid (empty response) — valid"
		case httpResp.StatusCode >= 300:
			res.Valid = false
			res.Findings = []openrtb.Finding{{Severity: openrtb.SevError, Field: "endpoint", Message: "endpoint returned HTTP " + httpResp.Status}}
		default:
			var resp openrtb.BidResponse
			if jerr := json.Unmarshal(respBody, &resp); jerr != nil {
				res.Valid = false
				res.Findings = []openrtb.Finding{{Severity: openrtb.SevError, Field: "", Message: "response is not a valid OpenRTB BidResponse JSON: " + jerr.Error()}}
			} else {
				res.Findings = openrtb.ValidateBidResponse(&golden, &resp)
				res.Valid = !openrtb.HasErrors(res.Findings)
			}
		}
		if res.LatencyMs > int64(p.TimeoutMs) && p.TimeoutMs > 0 {
			res.Findings = append(res.Findings, openrtb.Finding{Severity: openrtb.SevWarn, Field: "latency", Message: "response exceeded your configured timeout"})
		}
		if res.Findings == nil {
			res.Findings = []openrtb.Finding{}
		}
		log.Info("partner test-bid", "account_id", accountID, "endpoint", p.EndpointBid, "status", httpResp.StatusCode, "latency_ms", latency, "valid", res.Valid)
		_ = json.NewEncoder(w).Encode(res)
	}
}
