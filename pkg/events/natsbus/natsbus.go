// Package natsbus implements the EventBus interface using NATS JetStream.
//
// JetStream provides persistent, at-least-once delivery with consumer groups.
// Combined with the idempotent consumer wrapper, this gives exactly-once processing.
//
// Usage:
//
//	bus, err := natsbus.New("nats://localhost:4222", "tracker", logger)
//	bus.Publish(ctx, "adtech.events.impression", data)
//	bus.Subscribe(ctx, "adtech.events.impression", "reporting", handler)
package natsbus

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var (
	_ events.EventBus        = (*Bus)(nil)
	_ events.BatchSubscriber = (*Bus)(nil)
)

// Bus implements events.EventBus using NATS JetStream.
type Bus struct {
	conn           *nats.Conn
	js             jetstream.JetStream
	service        string
	log            *slog.Logger
	streamReplicas int
}

// streamReplicasFromEnv reads NATS_STREAM_REPLICAS (default 1). Local dev runs a
// single standalone NATS node, so streams are 1× (no raft). Staging/prod run a
// 3-node cluster and set this to 3 so a stream survives a node failure. Must be
// <= the NATS cluster size or CreateOrUpdateStream fails with insufficient peers.
func streamReplicasFromEnv() int {
	if v := os.Getenv("NATS_STREAM_REPLICAS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return 1
}

// New connects to NATS and returns a JetStream-backed EventBus.
func New(url, service string, log *slog.Logger) (*Bus, error) {
	nc, err := nats.Connect(url,
		nats.RetryOnFailedConnect(true),
		// Reconnect FOREVER. The old cap (30 × 1s) permanently closed the
		// connection after a ~30s NATS outage — every later publish failed
		// "nats: connection closed" until the pod was bounced. Observed
		// live: a laptop-sleep VM suspension outlasted the budget and the
		// report-runner silently stopped announcing completions. Outage
		// behaviour is unchanged (publishes fail while disconnected); the
		// difference is the client recovers when NATS does.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				log.Warn("nats disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			log.Info("nats reconnected")
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", url, err)
	}

	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("jetstream init: %w", err)
	}

	log.Info("nats connected", "url", url, "service", service)

	return &Bus{conn: nc, js: js, service: service, log: log, streamReplicas: streamReplicasFromEnv()}, nil
}

// EnsureStream creates a JetStream stream if it doesn't exist.
func (b *Bus) EnsureStream(ctx context.Context, name string, subjects []string) error {
	_, err := b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: jetstream.InterestPolicy, // keep until all consumers ack
		MaxAge:    24 * time.Hour,
		Storage:   jetstream.FileStorage,
		Replicas:  b.streamReplicas, // 1 local (standalone), 3 in a prod cluster (NATS_STREAM_REPLICAS)
		// Server-side dedup window for Nats-Msg-Id (PublishWithID): a client
		// republish after an ambiguous ack lands within seconds, so 2m is
		// generous while keeping the server's ID-tracking memory small.
		Duplicates: 2 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", name, err)
	}
	b.log.Info("stream ensured", "name", name, "subjects", subjects)
	return nil
}

func (b *Bus) Publish(ctx context.Context, subject string, data []byte) error {
	// Inject the active OTel span context as NATS message headers so the
	// consumer span links back to this publisher span. Without this, the
	// Jaeger trace breaks at every NATS hop — tracker emits a span, but
	// reporting's consumer work shows as a separate disconnected trace.
	msg := &nats.Msg{Subject: subject, Data: data, Header: nats.Header{}}
	otel.GetTextMapPropagator().Inject(ctx, natsHeaderCarrier(msg.Header))

	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

// PublishWithID is Publish with a Nats-Msg-Id header: JetStream drops a
// second entry with the same ID inside the stream's Duplicates window, so a
// client republish after an ambiguous ack can't double-enter the stream.
func (b *Bus) PublishWithID(ctx context.Context, subject, msgID string, data []byte) error {
	msg := &nats.Msg{Subject: subject, Data: data, Header: nats.Header{}}
	otel.GetTextMapPropagator().Inject(ctx, natsHeaderCarrier(msg.Header))
	msg.Header.Set("Nats-Msg-Id", msgID)
	if _, err := b.js.PublishMsg(ctx, msg); err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

func (b *Bus) Subscribe(ctx context.Context, subject, group string, handler events.Handler) error {
	// Consumer name must include the subject — without this, a caller that
	// subscribes to multiple subjects under the same group (e.g. reporting
	// subscribes to impression + click + conversion + auction.complete all
	// as group="reporting") collides: each CreateOrUpdateConsumer call
	// overwrites the previous FilterSubject, and only the last subject's
	// messages get delivered. The other handlers silently misdecode whatever
	// the "winning" subject's payloads happen to be.
	//
	// We sanitise the subject for use in a NATS consumer name (no dots).
	consumerName := fmt.Sprintf("%s-%s-%s", b.service, group, subjectToConsumerSuffix(subject))

	consumer, err := b.js.CreateOrUpdateConsumer(ctx, streamForSubject(subject), jetstream.ConsumerConfig{
		Name:          consumerName,
		Durable:       consumerName,
		FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    5,
	})
	if err != nil {
		return fmt.Errorf("create consumer %s on %s: %w", consumerName, subject, err)
	}

	// Start consuming in background
	go func() {
		for {
			msgs, err := consumer.Fetch(10, jetstream.FetchMaxWait(5*time.Second))
			if err != nil {
				if ctx.Err() != nil {
					return // context cancelled, shutting down
				}
				time.Sleep(time.Second)
				continue
			}

			for msg := range msgs.Messages() {
				// Extract the publisher's span context from NATS headers
				// (set by Publish). Then open a CONSUMER span as its child,
				// so Jaeger renders the full publisher → consumer flow as
				// one trace. ctx propagates the span context into the
				// handler — downstream code can call tracing.TraceIDFromContext
				// to get the same W3C trace ID the publisher saw.
				msgCtx := otel.GetTextMapPropagator().Extract(ctx, natsHeaderCarrier(msg.Headers()))
				msgCtx, span := otel.Tracer("adtech").Start(msgCtx,
					"nats consume "+msg.Subject(),
					trace.WithSpanKind(trace.SpanKindConsumer),
					trace.WithAttributes(
						attribute.String("messaging.system", "nats"),
						attribute.String("messaging.destination", msg.Subject()),
					),
				)

				traceID := ""
				if sc := span.SpanContext(); sc.HasTraceID() {
					traceID = sc.TraceID().String()
				}

				evtMsg := events.NewMessage(
					msg.Subject(),
					msg.Data(),
					traceID,
					msgID(msg),
					func() error { return msg.Ack() },
					func() error { return msg.Nak() },
				)

				if err := handler(msgCtx, evtMsg); err != nil {
					b.log.Error("handler failed", "subject", subject, "error", err)
					msg.Nak()
				}
				span.End()
			}
		}
	}()

	b.log.Info("subscribed", "subject", subject, "consumer", consumerName)
	return nil
}

// Batch-consumer tuning. A JetStream fetch already returns up to N messages;
// SubscribeBatch hands that whole slice to the handler as one unit so it can
// bulk-insert. Larger fetch + longer AckWait than the per-message path
// because a batch flush (one bulk INSERT) is the ack unit.
const (
	batchFetchSize     = 500
	batchFetchWait     = time.Second
	batchAckWait       = 60 * time.Second
	batchMaxAckPending = 2000
)

// SubscribeBatch is the bulk-consume path (events.BatchSubscriber). It uses
// the same durable-consumer conventions as Subscribe (subject-scoped name,
// explicit ack, MaxDeliver=5 → DLQ) but delivers each JetStream fetch to the
// handler as a slice. The handler owns ack/nak of every message; the bus
// does not ack/nak on its behalf (a batch handler needs per-message control
// to drop poison rows and redeliver good-but-unwritten ones).
func (b *Bus) SubscribeBatch(ctx context.Context, subject, group string, handler events.BatchHandler) error {
	consumerName := fmt.Sprintf("%s-%s-%s", b.service, group, subjectToConsumerSuffix(subject))

	consumer, err := b.js.CreateOrUpdateConsumer(ctx, streamForSubject(subject), jetstream.ConsumerConfig{
		Name:          consumerName,
		Durable:       consumerName,
		FilterSubject: subject,
		DeliverPolicy: jetstream.DeliverNewPolicy,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       batchAckWait,
		MaxAckPending: batchMaxAckPending,
		MaxDeliver:    5,
	})
	if err != nil {
		return fmt.Errorf("create batch consumer %s on %s: %w", consumerName, subject, err)
	}

	go func() {
		for {
			msgs, err := consumer.Fetch(batchFetchSize, jetstream.FetchMaxWait(batchFetchWait))
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				time.Sleep(time.Second)
				continue
			}

			batch := make([]*events.Message, 0, batchFetchSize)
			for msg := range msgs.Messages() {
				// Extract the publisher's trace context so each Message
				// carries the same W3C trace_id (no new span per message on
				// the batch path — the payload + logs carry trace_id, and
				// opening a span per row would defeat the batching win).
				msgCtx := otel.GetTextMapPropagator().Extract(ctx, natsHeaderCarrier(msg.Headers()))
				traceID := ""
				if sc := trace.SpanContextFromContext(msgCtx); sc.HasTraceID() {
					traceID = sc.TraceID().String()
				}
				m := msg // capture
				batch = append(batch, events.NewMessage(
					m.Subject(), m.Data(), traceID, msgID(m),
					func() error { return m.Ack() },
					func() error { return m.Nak() },
				))
			}
			if len(batch) == 0 {
				continue
			}
			if err := handler(ctx, batch); err != nil {
				b.log.Error("batch handler failed", "subject", subject, "n", len(batch), "error", err)
			}
		}
	}()

	b.log.Info("subscribed (batch)", "subject", subject, "consumer", consumerName, "fetch", batchFetchSize)
	return nil
}

func (b *Bus) Close() error {
	b.conn.Close()
	return nil
}

// msgID returns a stable dedup identity for a message. Prefer the publisher's
// Nats-Msg-Id header when set; otherwise fall back to the JetStream stream
// sequence, which is assigned at publish and is IDENTICAL across redeliveries
// of the same message (and unique across distinct messages) — exactly what
// consumer-side redelivery dedup needs. Publish doesn't currently set
// Nats-Msg-Id, so the stream sequence is the effective key.
func msgID(msg jetstream.Msg) string {
	if id := msg.Headers().Get("Nats-Msg-Id"); id != "" {
		return id
	}
	if md, err := msg.Metadata(); err == nil {
		return fmt.Sprintf("seq-%d", md.Sequence.Stream)
	}
	return ""
}

// subjectToConsumerSuffix turns a dotted subject into a NATS-safe consumer
// name suffix. Strips the "adtech." namespace prefix and replaces remaining
// dots with hyphens so the entire path is preserved:
//
//	adtech.events.impression          → events-impression
//	adtech.auction.win                → auction-win
//	adtech.direct.win                 → direct-win
//	adtech.prebid.outbound.win        → prebid-outbound-win
//
// Earlier this function used only the leaf segment. That collided across
// any pair of subjects sharing a leaf (e.g. every *.win subject became the
// consumer name "win"), with the consequence that CreateOrUpdateConsumer
// would overwrite the previous FilterSubject and only the last subscriber's
// subject's messages would actually be delivered. Reporting silently
// dropped DirectWin + PrebidOutboundWin events because both shared "win"
// with the older auction.win consumer.
func subjectToConsumerSuffix(subject string) string {
	const adtechPrefix = "adtech."
	if len(subject) > len(adtechPrefix) && subject[:len(adtechPrefix)] == adtechPrefix {
		subject = subject[len(adtechPrefix):]
	}
	out := []byte(subject)
	for i := range out {
		if out[i] == '.' {
			out[i] = '-'
		}
	}
	return string(out)
}

// streamForSubject maps a subject to its JetStream stream name.
func streamForSubject(subject string) string {
	// All adtech events go to the "adtech" stream
	return "adtech"
}

// natsHeaderCarrier adapts nats.Header to OTel's TextMapCarrier so the
// propagator can inject/extract W3C traceparent into NATS message headers.
// nats.Header is map[string][]string under the hood — same shape as
// http.Header but a distinct named type, hence the adapter.
type natsHeaderCarrier nats.Header

func (c natsHeaderCarrier) Get(key string) string {
	v := nats.Header(c).Get(key)
	return v
}

func (c natsHeaderCarrier) Set(key, value string) {
	nats.Header(c).Set(key, value)
}

func (c natsHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
