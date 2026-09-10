package alerts

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP speaks just enough of RFC 5321 to accept one message and hand it back.
//
// In-process rather than a mailpit container, deliberately. A transport test that needs Docker is a
// transport test that gets skipped on the machine where the transport is being written, and the
// thing under test here — that the client says HELO, MAIL, RCPT, DATA in the right order and
// produces a body with the right headers — needs no real mail server to observe.
//
// It advertises no STARTTLS, so these tests run with `tls: none`. The encrypted paths are exercised
// against a real server rather than a fake one; what is asserted here is the protocol conversation
// and the message.
type fakeSMTP struct {
	addr string

	mu       sync.Mutex
	commands []string
	body     string
	done     chan struct{}
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()

	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	s := &fakeSMTP{addr: listener.Addr().String(), done: make(chan struct{})}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		s.serve(conn)
		close(s.done)
	}()
	return s
}

func (s *fakeSMTP) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	write := func(line string) { _, _ = conn.Write([]byte(line + "\r\n")) }

	write("220 fake.example ESMTP")
	inData := false
	var body strings.Builder

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.body = body.String()
				s.mu.Unlock()
				write("250 2.0.0 Ok")
				continue
			}
			body.WriteString(line + "\n")
			continue
		}

		s.mu.Lock()
		s.commands = append(s.commands, line)
		s.mu.Unlock()

		switch {
		case strings.HasPrefix(line, "EHLO"):
			// No STARTTLS and no AUTH advertised: this fake is for the plaintext path.
			write("250-fake.example")
			write("250 8BITMIME")
		case strings.HasPrefix(line, "HELO"):
			write("250 fake.example")
		case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
			write("250 2.0.0 Ok")
		case line == "DATA":
			inData = true
			write("354 End data with <CR><LF>.<CR><LF>")
		case line == "QUIT":
			write("221 2.0.0 Bye")
			return
		default:
			write("502 5.5.2 Not implemented")
		}
	}
}

func (s *fakeSMTP) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the fake SMTP server never finished the conversation")
	}
}

func (s *fakeSMTP) received() (commands []string, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...), s.body
}

func TestSMTPDeliversAReadableMessage(t *testing.T) {
	server := startFakeSMTP(t)
	host, port, _ := net.SplitHostPort(server.addr)

	dest := destination{
		TenantID: "t", Name: "dba-oncall", Kind: NotifierSMTP,
		Settings: map[string]string{
			"host": host, "port": port, "tls": tlsNone,
			"from": "fleetward@example.com", "to": "dba@example.com, oncall@example.com",
		},
	}
	msg := Message{
		TenantID: "t", Kind: KindVerificationFailed, Severity: "critical", State: "firing",
		InstanceName: "prod-orders",
		Summary:      "A backup of prod-orders failed verification",
		Detail:       "The restored data did not match its manifest.",
		Fingerprint:  "verification_failed:b-1",
		FiredAt:      time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sendSMTP(ctx, dest, "", msg); err != nil {
		t.Fatalf("sendSMTP = %v", err)
	}
	server.wait(t)

	commands, body := server.received()
	joined := strings.Join(commands, "\n")
	// Both recipients, separately: a notifier addressed to two people that silently mails one is
	// the failure nobody notices until the wrong person is on call.
	if !strings.Contains(joined, "RCPT TO:<dba@example.com>") ||
		!strings.Contains(joined, "RCPT TO:<oncall@example.com>") {
		t.Errorf("both recipients were not offered:\n%s", joined)
	}
	if !strings.Contains(joined, "MAIL FROM:<fleetward@example.com>") {
		t.Errorf("the sender was not offered:\n%s", joined)
	}

	if !strings.Contains(body, "Subject: [Fleetward] CRITICAL: A backup of prod-orders failed verification") {
		t.Errorf("the subject does not lead with the severity:\n%s", body)
	}
	if !strings.Contains(body, "Fingerprint: verification_failed:b-1") {
		t.Errorf("the fingerprint is not in the body:\n%s", body)
	}
	// So a mail client threads the resolution under the alert rather than starting a second
	// conversation about the same broken thing.
	if !strings.Contains(body, "References: <verification_failed:b-1@fleetward>") {
		t.Errorf("the message carries no threading reference:\n%s", body)
	}
	if !strings.Contains(body, "The restored data did not match its manifest.") {
		t.Errorf("the detail is missing:\n%s", body)
	}
}

// TestSMTPRefusesToSendAPasswordInTheClear is the sharp edge of net/smtp, turned into an answer an
// operator can act on.
//
// `smtp.PlainAuth` refuses to authenticate over an unencrypted connection, which is correct and
// surprises people. Left to the standard library it fails at 3am with a message about an
// unencrypted connection; refused here, it fails when the notifier is created, saying which of the
// two settings to change.
func TestSMTPRefusesToSendAPasswordInTheClear(t *testing.T) {
	dest := destination{
		TenantID: "t", Kind: NotifierSMTP,
		Settings: map[string]string{
			"host": "mail.example.com", "tls": tlsNone,
			"from": "a@example.com", "to": "b@example.com", "username": "fleetward",
		},
	}
	err := sendSMTP(context.Background(), dest, "hunter2", Message{TenantID: "t"})
	if err == nil {
		t.Fatal("a username with tls: none was accepted; the password would go over the wire in the clear")
	}
	if !strings.Contains(err.Error(), "unencrypted") {
		t.Errorf("the error does not say why: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatal("the password is in the error message")
	}
}

func TestSMTPNeedsRecipients(t *testing.T) {
	dest := destination{
		TenantID: "t", Kind: NotifierSMTP,
		Settings: map[string]string{"host": "mail.example.com", "from": "a@example.com", "to": " , "},
	}
	if err := sendSMTP(context.Background(), dest, "", Message{TenantID: "t"}); err == nil {
		t.Fatal("a notifier whose recipient list is whitespace was accepted")
	}
}

func TestSMTPPortDefaultsFollowTheTLSMode(t *testing.T) {
	// 465 expects TLS from the first byte and 587 expects STARTTLS. Guessing one for the other is a
	// connection that hangs rather than one that fails, which is the worse of the two.
	if got := defaultSMTPPort(tlsImplicit); got != "465" {
		t.Errorf("implicit TLS defaults to port %s, want 465", got)
	}
	if got := defaultSMTPPort(tlsStartTLS); got != "587" {
		t.Errorf("STARTTLS defaults to port %s, want 587", got)
	}
}

func TestASubjectCannotCarryAHeaderInjection(t *testing.T) {
	msg := Message{
		State: "firing", Severity: "warning",
		Summary: "broken\r\nBcc: attacker@example.com",
	}
	if got := mailSubject(msg); strings.ContainsAny(got, "\r\n") {
		t.Fatalf("the subject carries a line break: %q", got)
	}
}

func TestRecipientsAreSplitAndTrimmed(t *testing.T) {
	got := splitRecipients(" a@example.com ,b@example.com,  , c@example.com ")
	want := []string{"a@example.com", "b@example.com", "c@example.com"}
	if len(got) != len(want) {
		t.Fatalf("splitRecipients = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitRecipients = %v, want %v", got, want)
		}
	}
}
