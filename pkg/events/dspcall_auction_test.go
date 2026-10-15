package events

import (
	"testing"
	"time"
)

func TestDSPCallAuctionFromLegs(t *testing.T) {
	if _, ok := DSPCallAuctionFromLegs(nil, time.Now()); ok {
		t.Fatal("empty legs must not produce an envelope")
	}
	t1 := time.Date(2026, 10, 15, 12, 0, 0, 1, time.UTC)
	legs := []DSPCallEvent{
		{TraceID: "tr", AuctionID: "tr", Channel: "video", DSPEndpoint: "http://a", BidReceived: true, BidPriceUSD: 2.5, LatencyMs: 30, Timestamp: t1},
		{TraceID: "tr", AuctionID: "tr", Channel: "video", DSPEndpoint: "http://b", TimedOut: true, LatencyMs: 500, NoBidReason: "fanout_deadline", Timestamp: t1.Add(time.Millisecond)},
	}
	now := t1.Add(time.Second)
	ev, ok := DSPCallAuctionFromLegs(legs, now)
	if !ok || ev.TraceID != "tr" || ev.Channel != "video" || !ev.Timestamp.Equal(now) || len(ev.Calls) != 2 {
		t.Fatalf("bad envelope: %+v", ev)
	}
	b := ev.Calls[1]
	if b.DSPEndpoint != "http://b" || !b.TimedOut || b.NoBidReason != "fanout_deadline" || b.LatencyMs != 500 || !b.Timestamp.Equal(t1.Add(time.Millisecond)) {
		t.Fatalf("leg fields not preserved: %+v", b)
	}
}
