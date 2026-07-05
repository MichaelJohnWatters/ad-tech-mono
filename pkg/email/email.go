// Package email provides email sending with pluggable backends.
// Uses Mailpit locally (fake SMTP with web UI) and SES/Sendgrid in production.
//
// Usage:
//
//	sender := email.NewSMTP("localhost:1025", "noreply@adtech.local", log) // Mailpit
//	sender.Send(ctx, email.Message{To: "user@example.com", Subject: "Welcome", Body: "..."})
package email

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

// Message is an email to send.
type Message struct {
	To       string
	From     string
	Subject  string
	Body     string
	HTML     string
	Template string // template name for rendering
	Data     map[string]interface{}
}

// Sender is the email sending interface.
type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// MemorySender stores emails in memory (for testing).
type MemorySender struct {
	mu   sync.Mutex
	sent []Message
	log  *slog.Logger
}

// NewMemory creates an in-memory email sender.
func NewMemory(log *slog.Logger) *MemorySender {
	return &MemorySender{log: log}
}

func (s *MemorySender) Send(_ context.Context, msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.From == "" {
		msg.From = "noreply@adtech.example"
	}
	s.sent = append(s.sent, msg)
	s.log.Info("email sent (memory)", "to", msg.To, "subject", msg.Subject)
	return nil
}

// Sent returns all sent emails.
func (s *MemorySender) Sent() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.sent))
	copy(out, s.sent)
	return out
}

// SMTPSender sends emails via SMTP (Mailpit locally, real SMTP in prod).
//
// host is "host:port" (e.g. Mailpit's "localhost:1025"). When auth is nil the
// connection is unauthenticated — the local Mailpit case. Set auth (via
// NewSMTPAuth) for providers that require login (SES/Sendgrid). now defaults to
// time.Now and is overridable for deterministic tests of the wire format.
type SMTPSender struct {
	host string
	from string
	auth smtp.Auth
	now  func() time.Time
	log  *slog.Logger
}

// NewSMTP creates an unauthenticated SMTP sender (Mailpit / open relay).
func NewSMTP(host, from string, log *slog.Logger) *SMTPSender {
	return &SMTPSender{host: host, from: from, now: time.Now, log: log}
}

// NewSMTPAuth creates an SMTP sender that authenticates with PLAIN credentials.
// Use for production providers (SES, Sendgrid). host is "host:port".
func NewSMTPAuth(host, from, username, password string, log *slog.Logger) *SMTPSender {
	hostOnly := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostOnly = h
	}
	return &SMTPSender{
		host: host,
		from: from,
		auth: smtp.PlainAuth("", username, password, hostOnly),
		now:  time.Now,
		log:  log,
	}
}

func (s *SMTPSender) Send(_ context.Context, msg Message) error {
	if msg.From == "" {
		msg.From = s.from
	}
	if msg.To == "" {
		return fmt.Errorf("email: missing recipient")
	}
	wire := s.buildMIME(msg)
	if err := smtp.SendMail(s.host, s.auth, msg.From, []string{msg.To}, wire); err != nil {
		s.log.Error("email send failed", "to", msg.To, "subject", msg.Subject, "host", s.host, "error", err)
		return fmt.Errorf("email: send to %s via %s: %w", msg.To, s.host, err)
	}
	s.log.Info("email sent (smtp)", "to", msg.To, "subject", msg.Subject, "host", s.host)
	return nil
}

// buildMIME renders an RFC 5322 message. If both Body (text) and HTML are set it
// emits multipart/alternative; if only one is set it emits that single part.
func (s *SMTPSender) buildMIME(msg Message) []byte {
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	var b strings.Builder
	writeHeader := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }

	writeHeader("From", msg.From)
	writeHeader("To", msg.To)
	writeHeader("Subject", msg.Subject)
	writeHeader("Date", now().Format(time.RFC1123Z))
	writeHeader("MIME-Version", "1.0")

	switch {
	case msg.Body != "" && msg.HTML != "":
		const boundary = "adtech-mixed-boundary-8f3a"
		writeHeader("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
		b.WriteString("\r\n")
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
		b.WriteString(msg.Body + "\r\n")
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: text/html; charset=UTF-8\r\n\r\n")
		b.WriteString(msg.HTML + "\r\n")
		b.WriteString("--" + boundary + "--\r\n")
	case msg.HTML != "":
		writeHeader("Content-Type", "text/html; charset=UTF-8")
		b.WriteString("\r\n")
		b.WriteString(msg.HTML + "\r\n")
	default:
		writeHeader("Content-Type", "text/plain; charset=UTF-8")
		b.WriteString("\r\n")
		b.WriteString(msg.Body + "\r\n")
	}
	return []byte(b.String())
}

// Templates for common emails
var Templates = map[string]string{
	"welcome": `Welcome to Ad Tech Platform!

Hi {{.Name}},

Your {{.AccountType}} account has been created.
Account ID: {{.AccountID}}

Get started: {{.DashboardURL}}
`,
	"invoice": `Invoice #{{.InvoiceID}}

Period: {{.PeriodStart}} - {{.PeriodEnd}}
Total: {{.Currency}} {{.Total}}
Due: {{.DueDate}}

View details: {{.InvoiceURL}}
`,
	"payout": `Payout #{{.PayoutID}}

Period: {{.PeriodStart}} - {{.PeriodEnd}}
Amount: {{.Currency}} {{.Amount}}
Payment terms: {{.PaymentTerms}}

View details: {{.PayoutURL}}
`,
	"budget_alert": `Budget Alert: {{.CampaignName}}

Your campaign "{{.CampaignName}}" has spent {{.SpentPct}}% of its budget.
Remaining: {{.Currency}} {{.Remaining}}

Manage: {{.CampaignURL}}
`,
}

// AuditEntry records an auditable action.
type AuditEntry struct {
	ID        int64
	Timestamp time.Time
	Actor     string // user_id or "system"
	AccountID string
	Action    string
	Resource  string
	Details   map[string]interface{}
}

// AuditLog stores audit entries.
type AuditLog struct {
	mu      sync.Mutex
	entries []AuditEntry
	nextID  int64
}

// NewAuditLog creates an in-memory audit log.
func NewAuditLog() *AuditLog {
	return &AuditLog{nextID: 1}
}

// Record adds an audit entry.
func (a *AuditLog) Record(entry AuditEntry) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry.ID = a.nextID
	a.nextID++
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	a.entries = append(a.entries, entry)
	return entry.ID
}

// Query returns entries matching the filter.
func (a *AuditLog) Query(accountID, actor, action string, limit int) []AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()

	var result []AuditEntry
	for i := len(a.entries) - 1; i >= 0 && len(result) < limit; i-- {
		e := a.entries[i]
		if accountID != "" && e.AccountID != accountID {
			continue
		}
		if actor != "" && e.Actor != actor {
			continue
		}
		if action != "" && e.Action != action {
			continue
		}
		result = append(result, e)
	}
	return result
}

// Count returns the total number of entries.
func (a *AuditLog) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.entries)
}

// DeploymentEntry records a service deployment.
type DeploymentEntry struct {
	Service    string
	Version    string
	Timestamp  time.Time
	CommitSHA  string
	DeployedBy string
}

// DeploymentLedger tracks all deployments for Grafana annotations.
type DeploymentLedger struct {
	mu      sync.Mutex
	entries []DeploymentEntry
}

// NewDeploymentLedger creates a deployment ledger.
func NewDeploymentLedger() *DeploymentLedger {
	return &DeploymentLedger{}
}

// Record adds a deployment entry.
func (d *DeploymentLedger) Record(entry DeploymentEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now()
	}
	d.entries = append(d.entries, entry)
}

// Recent returns the N most recent deployments.
func (d *DeploymentLedger) Recent(n int) []DeploymentEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	start := len(d.entries) - n
	if start < 0 {
		start = 0
	}
	result := make([]DeploymentEntry, len(d.entries)-start)
	copy(result, d.entries[start:])
	return result
}

// ForService returns deployments for a specific service.
func (d *DeploymentLedger) ForService(service string) []DeploymentEntry {
	d.mu.Lock()
	defer d.mu.Unlock()
	var result []DeploymentEntry
	for _, e := range d.entries {
		if e.Service == service {
			result = append(result, e)
		}
	}
	return result
}
