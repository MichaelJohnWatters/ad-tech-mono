package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/email"
	"github.com/MichaelJohnWatters/ad-tech-mono/pkg/ingestjobs"
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestNotifyResultSuccess(t *testing.T) {
	sender := email.NewMemory(testLog())
	job := ingestjobs.Job{
		ID:           "job-1",
		SegmentSpec:  ingestjobs.SegmentSpec{Name: "vips"},
		NotifyEmails: []string{"a@example.com", "b@example.com"},
	}
	res := ingestjobs.IngestResult{SegmentID: "seg-1", MembersAdded: 7, ValidRows: 9, MatchedRows: 5, MatchRate: 0.5}

	NotifyResult(context.Background(), sender, "from@adtech.local", job, res, nil, testLog())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sent %d emails, want 2", len(sent))
	}
	if !strings.Contains(sent[0].Subject, "succeeded") || !strings.Contains(sent[0].Subject, "vips") {
		t.Errorf("subject = %q", sent[0].Subject)
	}
	if !strings.Contains(sent[0].Body, "Members added: 7") {
		t.Errorf("body missing members added: %q", sent[0].Body)
	}
}

func TestNotifyResultFailure(t *testing.T) {
	sender := email.NewMemory(testLog())
	job := ingestjobs.Job{
		SegmentSpec:  ingestjobs.SegmentSpec{Name: "bad-file"},
		NotifyEmails: []string{"a@example.com"},
	}
	NotifyResult(context.Background(), sender, "from@adtech.local", job,
		ingestjobs.IngestResult{}, errors.New("no usable id column"), testLog())

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d emails, want 1", len(sent))
	}
	if !strings.Contains(sent[0].Subject, "failed") {
		t.Errorf("subject = %q", sent[0].Subject)
	}
	if !strings.Contains(sent[0].Body, "no usable id column") {
		t.Errorf("body missing reason: %q", sent[0].Body)
	}
}

func TestNotifyResultNoRecipientsNoSend(t *testing.T) {
	sender := email.NewMemory(testLog())
	job := ingestjobs.Job{SegmentSpec: ingestjobs.SegmentSpec{Name: "x"}}
	NotifyResult(context.Background(), sender, "from", job, ingestjobs.IngestResult{}, nil, testLog())
	if n := len(sender.Sent()); n != 0 {
		t.Errorf("sent %d emails with no recipients, want 0", n)
	}
	// Nil sender is a safe no-op.
	NotifyResult(context.Background(), nil, "from",
		ingestjobs.Job{NotifyEmails: []string{"a@example.com"}}, ingestjobs.IngestResult{}, nil, testLog())
}

func TestSanitizeNotifyEmails(t *testing.T) {
	got := SanitizeNotifyEmails([]string{" a@example.com ", "a@example.com", "not-an-email", "", "b@example.com"})
	want := []string{"a@example.com", "b@example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
