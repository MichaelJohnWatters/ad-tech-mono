package email

import (
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func testSender() *SMTPSender {
	s := NewSMTP("localhost:1025", "noreply@adtech.local", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestBuildMIME_PlainText(t *testing.T) {
	s := testSender()
	wire := string(s.buildMIME(Message{From: "a@x.test", To: "b@y.test", Subject: "Hi", Body: "hello"}))

	for _, want := range []string{
		"From: a@x.test\r\n",
		"To: b@y.test\r\n",
		"Subject: Hi\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
		"MIME-Version: 1.0\r\n",
		"\r\nhello\r\n",
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %q\n---\n%s", want, wire)
		}
	}
	if strings.Contains(wire, "multipart") {
		t.Errorf("plain-text message should not be multipart:\n%s", wire)
	}
}

func TestBuildMIME_Multipart(t *testing.T) {
	s := testSender()
	wire := string(s.buildMIME(Message{
		From: "a@x.test", To: "b@y.test", Subject: "Report",
		Body: "plain body", HTML: "<h1>rich</h1>",
	}))

	for _, want := range []string{
		"Content-Type: multipart/alternative; boundary=",
		"Content-Type: text/plain; charset=UTF-8\r\n\r\nplain body\r\n",
		"Content-Type: text/html; charset=UTF-8\r\n\r\n<h1>rich</h1>\r\n",
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire missing %q\n---\n%s", want, wire)
		}
	}
	// boundary must be opened twice and closed once.
	if got := strings.Count(wire, "--adtech-mixed-boundary-8f3a"); got != 3 {
		t.Errorf("boundary markers = %d, want 3\n%s", got, wire)
	}
}

func TestBuildMIME_HTMLOnly(t *testing.T) {
	s := testSender()
	wire := string(s.buildMIME(Message{From: "a@x.test", To: "b@y.test", Subject: "H", HTML: "<p>x</p>"}))
	if !strings.Contains(wire, "Content-Type: text/html; charset=UTF-8\r\n") {
		t.Errorf("expected html content-type:\n%s", wire)
	}
	if strings.Contains(wire, "multipart") {
		t.Errorf("html-only should not be multipart:\n%s", wire)
	}
}

func TestSend_MissingRecipient(t *testing.T) {
	s := testSender()
	if err := s.Send(nil, Message{Subject: "x", Body: "y"}); err == nil {
		t.Fatal("expected error for missing recipient")
	}
}

func TestSend_DeliversToSMTP(t *testing.T) {
	// Stand up a throwaway SMTP server that captures the DATA payload, then
	// assert Send drives a real SMTP conversation end-to-end (no mocking of net/smtp).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	dataCh := make(chan string, 1)
	go serveOneSMTP(t, ln, dataCh)

	s := NewSMTP(ln.Addr().String(), "noreply@adtech.local", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Send(nil, Message{To: "user@example.com", Subject: "Scheduled report", Body: "your report"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case data := <-dataCh:
		if !strings.Contains(data, "Subject: Scheduled report") || !strings.Contains(data, "your report") {
			t.Errorf("delivered payload missing content:\n%s", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SMTP delivery")
	}
}

// serveOneSMTP is a minimal SMTP server: it accepts one connection, walks the
// SMTP dialogue enough to receive the DATA body, and publishes it to ch.
func serveOneSMTP(t *testing.T, ln net.Listener, ch chan<- string) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	buf := make([]byte, 4096)
	write := func(s string) { _, _ = conn.Write([]byte(s)) }
	read := func() string {
		n, _ := conn.Read(buf)
		return string(buf[:n])
	}

	write("220 test ESMTP\r\n")
	var body strings.Builder
	inData := false
	for {
		line := read()
		if line == "" {
			return
		}
		switch {
		case inData:
			if strings.Contains(line, "\r\n.\r\n") {
				body.WriteString(strings.SplitN(line, "\r\n.\r\n", 2)[0])
				write("250 OK\r\n")
				ch <- body.String()
				inData = false
			} else {
				body.WriteString(line)
			}
		case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
			write("250 test\r\n")
		case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
			write("250 OK\r\n")
		case strings.HasPrefix(line, "DATA"):
			write("354 End data with <CR><LF>.<CR><LF>\r\n")
			inData = true
		case strings.HasPrefix(line, "QUIT"):
			write("221 Bye\r\n")
			return
		default:
			write("250 OK\r\n")
		}
	}
}
