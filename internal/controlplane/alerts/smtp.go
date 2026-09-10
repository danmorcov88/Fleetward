package alerts

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// The TLS modes an SMTP notifier may declare.
//
// Three rather than "figure it out", because guessing here fails in the worst possible way: a
// notifier that silently falls back to plaintext would send an alert — and its credential — in the
// clear on a network the operator believed was encrypted.
const (
	// tlsStartTLS connects in the clear on the submission port and upgrades. The common case.
	tlsStartTLS = "starttls"
	// tlsImplicit connects with TLS from the first byte, which is what port 465 expects.
	tlsImplicit = "implicit"
	// tlsNone is plaintext, for a relay on localhost that does not offer TLS. Authentication is
	// refused in this mode — see below.
	tlsNone = "none"
)

// sendSMTP delivers one notification by mail.
//
// Written against net/smtp's two sharp edges rather than around them:
//
// **`smtp.PlainAuth` refuses to authenticate over an unencrypted connection**, which is correct and
// surprises people. So `tls: none` with a username configured is refused here, with a message that
// says which of the two to change, rather than failing at 3am inside the standard library.
//
// **`smtp.SendMail` cannot do implicit TLS.** Port 465 expects TLS from the first byte, and
// SendMail dials in the clear and offers STARTTLS. That is why this dials the connection itself in
// every mode instead of taking the convenient path in two of them and a different one in the third.
func sendSMTP(ctx context.Context, dest destination, secret string, msg Message) error {
	var (
		host     = dest.Settings["host"]
		port     = dest.Settings["port"]
		from     = dest.Settings["from"]
		to       = splitRecipients(dest.Settings["to"])
		username = dest.Settings["username"]
		mode     = dest.Settings["tls"]
	)
	if mode == "" {
		mode = tlsStartTLS
	}
	if port == "" {
		port = defaultSMTPPort(mode)
	}
	if len(to) == 0 {
		return fmt.Errorf("this smtp notifier has no recipients in its settings")
	}
	if username != "" && mode == tlsNone {
		return fmt.Errorf(
			"this smtp notifier has a username but tls is %q, and a password must not be sent over "+
				"an unencrypted connection; set tls to %q or remove the username",
			tlsNone, tlsStartTLS)
	}

	addr := net.JoinHostPort(host, port)
	dialer := &net.Dialer{Timeout: 15 * time.Second}

	var (
		conn net.Conn
		err  error
	)
	if mode == tlsImplicit {
		// tls.Dialer rather than tls.DialWithDialer, so the handshake is bound to the delivery
		// attempt's deadline like every other step. A TLS handshake with a server that accepts the
		// connection and then says nothing is otherwise unbounded.
		tlsDialer := &tls.Dialer{
			NetDialer: dialer,
			Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		}
		conn, err = tlsDialer.DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("the smtp server at %s could not be reached", addr)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("the smtp server at %s did not greet us: %w", addr, err)
	}
	defer func() { _ = client.Close() }()

	if mode == tlsStartTLS {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return fmt.Errorf(
				"the smtp server at %s does not offer STARTTLS; set tls to %q if that is expected",
				addr, tlsNone)
		}
		if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("the smtp server at %s refused STARTTLS: %w", addr, err)
		}
	}

	if username != "" {
		if err := client.Auth(smtp.PlainAuth("", username, secret, host)); err != nil {
			// The library's message names the mechanism and the server's reply, never the password.
			return fmt.Errorf("the smtp server at %s refused the credential: %w", addr, err)
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("the smtp server rejected the sender %q: %w", from, err)
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return fmt.Errorf("the smtp server rejected the recipient %q: %w", recipient, err)
		}
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("the smtp server refused the message body: %w", err)
	}
	if _, err := w.Write([]byte(mailBody(from, to, msg))); err != nil {
		return fmt.Errorf("could not write the message body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the smtp server rejected the message: %w", err)
	}
	return client.Quit()
}

// mailBody renders one plain-text message.
//
// Plain text and one fixed shape, for the same reason the webhook payload is: templating is a
// feature request rather than a slice, and there is nothing here a recipient could not already read
// through the API.
func mailBody(from string, to []string, msg Message) string {
	var b strings.Builder

	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	b.WriteString("Subject: " + mailSubject(msg) + "\r\n")
	b.WriteString("Date: " + msg.FiredAt.Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	// So a mail client threads a resolution under the alert it resolves rather than starting a
	// second conversation about the same broken thing.
	b.WriteString("References: <" + msg.Fingerprint + "@fleetward>\r\n")
	b.WriteString("\r\n")

	b.WriteString(msg.Summary + "\r\n\r\n")
	if msg.Detail != "" {
		b.WriteString(msg.Detail + "\r\n\r\n")
	}
	b.WriteString("State:       " + msg.State + "\r\n")
	b.WriteString("Severity:    " + msg.Severity + "\r\n")
	b.WriteString("Rule kind:   " + msg.Kind + "\r\n")
	if msg.InstanceName != "" {
		b.WriteString("Instance:    " + msg.InstanceName + "\r\n")
	}
	b.WriteString("Fingerprint: " + msg.Fingerprint + "\r\n")
	b.WriteString("\r\n-- \r\nSent by Fleetward.\r\n")
	return b.String()
}

func mailSubject(msg Message) string {
	prefix := "[Fleetward] "
	switch msg.State {
	case "resolved":
		prefix += "RESOLVED: "
	case "test":
		prefix += "TEST: "
	default:
		prefix += strings.ToUpper(msg.Severity) + ": "
	}
	// Header injection is impossible from a summary the product wrote, and stripping the two
	// characters that would enable it costs nothing and does not depend on that staying true.
	return prefix + strings.NewReplacer("\r", " ", "\n", " ").Replace(msg.Summary)
}

func splitRecipients(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func defaultSMTPPort(mode string) string {
	if mode == tlsImplicit {
		return "465"
	}
	return "587"
}
