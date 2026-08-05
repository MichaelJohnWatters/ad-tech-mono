package events

// Disk spool: the fix for the exchange→NATS hop being at-most-once.
//
// 2026-08-05: a VM seizure bounced nats-0 three times mid-soak; every publish
// in flight timed out and was dropped — 146,757 "failed to publish event" log
// lines, ~94k auctions + ~24k impressions permanently lost, simulator --verify
// red. "Exactly-once" (Nats-Msg-Id + reporting's biz-key dedup) only ever held
// DOWNSTREAM of NATS.
//
// The spool closes the producer half: a failed publish appends the event to a
// local append-only file instead of dropping it, and a background drainer
// republishes once NATS answers again. Replays are safe because the message ID
// is part of the spooled record — money events carry stable trace-derived IDs
// ("win:"+trace) and reporting also dedupes on business keys, so a replay
// cannot double-bill. Events spooled without an ID get one minted at append
// time so even telemetry replays are dedup-guarded within the stream's window.
//
// Deployment shape: EVENT_SPOOL_DIR should point at an emptyDir volume —
// emptyDir survives CONTAINER restarts (what probe-kill storms actually do),
// which is the failure mode observed. Pod EVICTION loses the spool; that
// residual window is a documented, deliberate trade — see docs/PLAN.md →
// "Build Status & Outstanding Work" → "Event-spool residual" for the full
// analysis (PVC vs transactional-outbox options, costs, and the revisit
// triggers). Monitor: spool_bytes > 0 during an eviction = at-risk events.
//
// Pressure (spool fill fraction) feeds the front-door throttle: the SSP sheds
// incoming serve requests as pressure rises, so the spool drains instead of
// growing without bound. See cmd/ssp's shed middleware and the exchange's
// X-Event-Pressure response header.

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// DefaultSpoolCap bounds the spool file. 256MiB ≈ 25+ minutes of full-mix
// event traffic at 150rps — far longer than any observed NATS outage.
const DefaultSpoolCap int64 = 256 << 20

// SpoolDirFromEnv is the conventional spool location: EVENT_SPOOL_DIR (helm
// mounts an emptyDir there so the spool survives container restarts), falling
// back to a tmp path so host runs and tests work without wiring.
func SpoolDirFromEnv() string {
	if d := os.Getenv("EVENT_SPOOL_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "adtech-event-spool")
}

// spoolRecord is one JSONL line in the spool file.
type spoolRecord struct {
	Subject string `json:"s"`
	MsgID   string `json:"i"`
	Data    string `json:"d"` // base64 of the marshalled event
}

// Spool is an append-only on-disk buffer of events that failed to publish.
// Concurrency-safe. One Spool per process (per Publisher).
type Spool struct {
	mu       sync.Mutex
	path     string
	cursor   string // sidecar file persisting the drain offset
	f        *os.File
	w        *bufio.Writer
	size     int64 // bytes appended and not yet truncated
	offset   int64 // bytes already drained (persisted in cursor file)
	capBytes int64

	spooled prometheus.Counter
	drained prometheus.Counter
	dropped prometheus.Counter
	gauge   prometheus.Gauge
}

// NewSpool opens (or resumes) a spool at dir/events.spool. capBytes bounds the
// file; appends past the cap are DROPPED (counted) — the front-door throttle
// exists precisely to keep the spool away from its cap. reg may be nil.
func NewSpool(dir string, capBytes int64, reg *prometheus.Registry) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("spool dir: %w", err)
	}
	s := &Spool{
		path:     filepath.Join(dir, "events.spool"),
		cursor:   filepath.Join(dir, "events.cursor"),
		capBytes: capBytes,
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("spool open: %w", err)
	}
	s.f, s.w = f, bufio.NewWriter(f)
	if st, err := f.Stat(); err == nil {
		s.size = st.Size()
	}
	if b, err := os.ReadFile(s.cursor); err == nil {
		fmt.Sscanf(string(b), "%d", &s.offset)
		if s.offset > s.size {
			s.offset = 0 // cursor from a previous, since-truncated file
		}
	}
	if reg != nil {
		c := func(name, help string) prometheus.Counter {
			ctr := prometheus.NewCounter(prometheus.CounterOpts{Namespace: "adtech", Subsystem: "events", Name: name, Help: help})
			reg.MustRegister(ctr)
			return ctr
		}
		s.spooled = c("spooled_total", "Events appended to the disk spool after a failed publish.")
		s.drained = c("drained_total", "Spooled events successfully republished to NATS.")
		s.dropped = c("dropped_total", "Events dropped because the spool hit its byte cap — DATA LOSS; the front-door throttle should prevent this.")
		s.gauge = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: "adtech", Subsystem: "events", Name: "spool_bytes", Help: "Un-drained bytes in the event spool."})
		reg.MustRegister(s.gauge)
	}
	return s, nil
}

// Append stores one failed event. Returns false when the spool is at cap (the
// event is dropped and counted).
func (s *Spool) Append(subject, msgID string, data []byte) bool {
	if msgID == "" {
		// Mint an ID so a replay is still deduped inside the stream's
		// Duplicates window (telemetry events publish without one).
		msgID = "spool:" + uuid.NewString()
	}
	line, err := json.Marshal(spoolRecord{Subject: subject, MsgID: msgID, Data: base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.size-s.offset+int64(len(line))+1 > s.capBytes {
		if s.dropped != nil {
			s.dropped.Inc()
		}
		return false
	}
	s.w.Write(line)
	s.w.WriteByte('\n')
	s.w.Flush()
	s.size += int64(len(line)) + 1
	if s.spooled != nil {
		s.spooled.Inc()
	}
	if s.gauge != nil {
		s.gauge.Set(float64(s.size - s.offset))
	}
	return true
}

// Pressure is the spool fill fraction as 0–100.
func (s *Spool) Pressure() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.capBytes == 0 {
		return 0
	}
	pending := s.size - s.offset
	p := int(pending * 100 / s.capBytes)
	if p == 0 && pending > 0 {
		p = 1 // pending-but-tiny must never read as "empty"
	}
	if p > 100 {
		p = 100
	}
	return p
}

// drainBatch republishes up to n spooled events via publish. Stops (keeping
// the remainder) on the first error — NATS is still down. When everything has
// drained, the file is truncated so it never grows unboundedly.
func (s *Spool) drainBatch(ctx context.Context, n int, publish func(ctx context.Context, subject, msgID string, data []byte) error) (int, error) {
	s.mu.Lock()
	if s.size == s.offset {
		s.mu.Unlock()
		return 0, nil
	}
	f, err := os.Open(s.path)
	if err != nil {
		s.mu.Unlock()
		return 0, err
	}
	if _, err := f.Seek(s.offset, 0); err != nil {
		f.Close()
		s.mu.Unlock()
		return 0, err
	}
	s.mu.Unlock()
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	drained := 0
	advanced := int64(0)
	for drained < n && sc.Scan() {
		line := sc.Bytes()
		var rec spoolRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			advanced += int64(len(line)) + 1 // skip corrupt line
			continue
		}
		data, err := base64.StdEncoding.DecodeString(rec.Data)
		if err != nil {
			advanced += int64(len(line)) + 1
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = publish(pctx, rec.Subject, rec.MsgID, data)
		cancel()
		if err != nil {
			break // NATS still down; keep the rest for next tick
		}
		advanced += int64(len(line)) + 1
		drained++
		if s.drained != nil {
			s.drained.Inc()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.offset += advanced
	os.WriteFile(s.cursor, []byte(fmt.Sprintf("%d", s.offset)), 0o644)
	if s.offset >= s.size {
		// Fully drained → truncate so the file (and the next boot's replay
		// window) stays small.
		s.w.Flush()
		s.f.Truncate(0)
		s.f.Seek(0, 0)
		s.size, s.offset = 0, 0
		os.WriteFile(s.cursor, []byte("0"), 0o644)
	}
	if s.gauge != nil {
		s.gauge.Set(float64(s.size - s.offset))
	}
	return drained, nil
}
