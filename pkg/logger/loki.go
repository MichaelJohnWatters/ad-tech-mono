package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// lokiSink is an io.Writer that pushes log lines to Loki's HTTP push API.
//
// Used as a dual-sink with stdout via io.MultiWriter so existing log call
// sites stay unchanged. The writer queues incoming records, batches them
// every flushInterval (or when batchSize is hit), and POSTs to Loki.
//
// Failure modes:
//   - Loki down: writes succeed (we never block log callers), batches drop
//     after the queue fills. A throttled stderr message warns.
//   - Loki slow: same; the queue acts as a small buffer.
//   - Process exit: a final flush attempt is made via Stop() so the
//     in-flight batch isn't lost.
type lokiSink struct {
	url       string
	labels    map[string]string
	queue     chan []byte
	stop      chan struct{}
	doneFlush chan struct{}

	mu       sync.Mutex
	lastWarn time.Time
	httpC    *http.Client
}

const (
	lokiQueueSize     = 4096 // dropped if full; tune for log volume
	lokiBatchSize     = 100  // lines per push
	lokiFlushInterval = 500 * time.Millisecond
	lokiPushPath      = "/loki/api/v1/push"
	lokiWarnThrottle  = 30 * time.Second
)

// newLokiSink starts a background pusher goroutine. Call Stop() on
// shutdown to drain.
func newLokiSink(url string, labels map[string]string) *lokiSink {
	s := &lokiSink{
		url:       strings.TrimRight(url, "/"),
		labels:    labels,
		queue:     make(chan []byte, lokiQueueSize),
		stop:      make(chan struct{}),
		doneFlush: make(chan struct{}),
		httpC:     &http.Client{Timeout: 3 * time.Second},
	}
	go s.loop()
	return s
}

// Write is the io.Writer side. Called once per slog record (one JSON
// line). Non-blocking: drops with a throttled warn if the queue is full.
func (s *lokiSink) Write(p []byte) (int, error) {
	// Clone because slog reuses the underlying buffer across calls.
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case s.queue <- cp:
	default:
		s.warnDropped()
	}
	return len(p), nil
}

func (s *lokiSink) warnDropped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastWarn) < lokiWarnThrottle {
		return
	}
	s.lastWarn = time.Now()
	fmt.Fprintf(os.Stderr, "logger: loki queue full, dropping records (throttle %s)\n", lokiWarnThrottle)
}

// loop drains the queue, batches, and posts. Exits on Stop signal after
// draining the queue once more.
func (s *lokiSink) loop() {
	defer close(s.doneFlush)
	ticker := time.NewTicker(lokiFlushInterval)
	defer ticker.Stop()
	batch := make([][]byte, 0, lokiBatchSize)

	for {
		select {
		case <-s.stop:
			// Drain whatever's queued, then send and exit.
			for {
				select {
				case b := <-s.queue:
					batch = append(batch, b)
					if len(batch) >= lokiBatchSize {
						s.push(batch)
						batch = batch[:0]
					}
				default:
					if len(batch) > 0 {
						s.push(batch)
					}
					return
				}
			}
		case b := <-s.queue:
			batch = append(batch, b)
			if len(batch) >= lokiBatchSize {
				s.push(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				s.push(batch)
				batch = batch[:0]
			}
		}
	}
}

// push converts the batch to Loki's stream format and POSTs it.
// Failures are logged to stderr (not slog — would recurse) and dropped.
func (s *lokiSink) push(batch [][]byte) {
	if len(batch) == 0 {
		return
	}

	// Loki accepts a JSON body shaped like:
	//   {"streams":[{"stream":{label:value,...},"values":[[ts_ns,"line"],...]}]}
	// Single stream is fine; labels are constant for this service instance.
	values := make([][2]string, 0, len(batch))
	for _, line := range batch {
		ts := extractTimestamp(line)
		// Strip trailing newline so it doesn't end up as literal \n in Loki UI.
		trimmed := bytes.TrimRight(line, "\n")
		values = append(values, [2]string{ts, string(trimmed)})
	}

	body := struct {
		Streams []stream `json:"streams"`
	}{
		Streams: []stream{{Labels: s.labels, Values: values}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+lokiPushPath, bytes.NewReader(raw))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpC.Do(req)
	if err != nil {
		// Throttled stderr warn — same pattern as queue-full.
		s.warnPush(err.Error())
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		s.warnPush(fmt.Sprintf("status %d", resp.StatusCode))
	}
}

func (s *lokiSink) warnPush(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastWarn) < lokiWarnThrottle {
		return
	}
	s.lastWarn = time.Now()
	fmt.Fprintf(os.Stderr, "logger: loki push failed: %s (throttle %s)\n", reason, lokiWarnThrottle)
}

// Stop signals the pusher to drain and exit. Blocks up to a small budget
// so the final batch ships before the process dies.
func (s *lokiSink) Stop() {
	close(s.stop)
	select {
	case <-s.doneFlush:
	case <-time.After(3 * time.Second):
	}
}

// stream is the Loki wire format for one set of labels + values.
type stream struct {
	Labels map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// extractTimestamp pulls "time":"..." out of a slog JSON record and
// converts to nanoseconds-since-epoch as a decimal string. Falls back
// to wall-clock now() if absent or malformed.
func extractTimestamp(line []byte) string {
	const key = `"time":"`
	idx := bytes.Index(line, []byte(key))
	if idx < 0 {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	start := idx + len(key)
	end := bytes.IndexByte(line[start:], '"')
	if end < 0 {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	tsStr := string(line[start : start+end])
	t, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d", t.UnixNano())
}
