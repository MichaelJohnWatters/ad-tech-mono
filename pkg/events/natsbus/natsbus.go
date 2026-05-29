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
	"time"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/events"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Bus implements events.EventBus using NATS JetStream.
type Bus struct {
	conn    *nats.Conn
	js      jetstream.JetStream
	service string
	log     *slog.Logger
}

// New connects to NATS and returns a JetStream-backed EventBus.
func New(url, service string, log *slog.Logger) (*Bus, error) {
	nc, err := nats.Connect(url,
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(30),
		nats.ReconnectWait(time.Second),
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

	return &Bus{conn: nc, js: js, service: service, log: log}, nil
}

// EnsureStream creates a JetStream stream if it doesn't exist.
func (b *Bus) EnsureStream(ctx context.Context, name string, subjects []string) error {
	_, err := b.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      name,
		Subjects:  subjects,
		Retention: jetstream.InterestPolicy, // keep until all consumers ack
		MaxAge:    24 * time.Hour,
		Storage:   jetstream.FileStorage,
		Replicas:  1, // single replica for local dev
	})
	if err != nil {
		return fmt.Errorf("create stream %s: %w", name, err)
	}
	b.log.Info("stream ensured", "name", name, "subjects", subjects)
	return nil
}

func (b *Bus) Publish(ctx context.Context, subject string, data []byte) error {
	_, err := b.js.Publish(ctx, subject, data)
	if err != nil {
		return fmt.Errorf("publish %s: %w", subject, err)
	}
	return nil
}

func (b *Bus) Subscribe(ctx context.Context, subject, group string, handler events.Handler) error {
	consumerName := fmt.Sprintf("%s-%s", b.service, group)

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
				evtMsg := events.NewMessage(
					msg.Subject(),
					msg.Data(),
					"", // trace ID extracted by handler
					msg.Headers().Get("Nats-Msg-Id"),
					func() error { return msg.Ack() },
					func() error { return msg.Nak() },
				)

				if err := handler(ctx, evtMsg); err != nil {
					b.log.Error("handler failed", "subject", subject, "error", err)
					msg.Nak()
				}
			}
		}
	}()

	b.log.Info("subscribed", "subject", subject, "consumer", consumerName)
	return nil
}

func (b *Bus) Close() error {
	b.conn.Close()
	return nil
}

// streamForSubject maps a subject to its JetStream stream name.
func streamForSubject(subject string) string {
	// All adtech events go to the "adtech" stream
	return "adtech"
}
