package alerts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTP sends through any SMTP server (Gmail with an app password, Outlook,
// Zoho, Mailgun, SES, ...). Configured from the environment:
//
//	SMTP_HOST, SMTP_PORT (587 = STARTTLS, 465 = TLS from the start),
//	SMTP_USERNAME, SMTP_PASSWORD, EMAIL_FROM (defaults to SMTP_USERNAME)
//
// The connection is always encrypted before the password is sent.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string // an address, or "Name <address>"
	Timeout  time.Duration

	// insecure skips TLS; only for tests against a local fake server.
	insecure bool
}

var errHeader = errors.New("smtp: header value contains a line break")

func headerSafe(vals ...string) error {
	for _, v := range vals {
		if strings.ContainsAny(v, "\r\n") {
			return errHeader
		}
	}
	return nil
}

// build makes the RFC 5322 message: plain text, plus HTML when given.
func (s *SMTP) build(from *mail.Address, m Message, now time.Time) []byte {
	var b bytes.Buffer
	host := s.Host
	if at := strings.LastIndexByte(from.Address, '@'); at >= 0 {
		host = from.Address[at+1:]
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	fmt.Fprintf(&b, "From: %s\r\n", from.String())
	fmt.Fprintf(&b, "To: %s\r\n", m.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", m.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), host)
	b.WriteString("MIME-Version: 1.0\r\n")
	text := strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")
	if m.HTML == "" {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(text)
		b.WriteString("\r\n")
		return b.Bytes()
	}
	boundary := "fa-" + hex.EncodeToString(id)
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n", boundary, text)
	html := strings.ReplaceAll(strings.ReplaceAll(m.HTML, "\r\n", "\n"), "\n", "\r\n")
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n", boundary, html)
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

func (s *SMTP) Send(ctx context.Context, m Message) error {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("smtp: EMAIL_FROM: %w", err)
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("smtp: recipient: %w", err)
	}
	if err := headerSafe(m.To, m.Subject); err != nil {
		return err
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	dialer := &net.Dialer{}
	var conn net.Conn
	if s.Port == 465 && !s.insecure {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp: connect %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	defer c.Close()
	if s.Port != 465 && !s.insecure {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: the server doesn't offer STARTTLS; refusing to send the password unencrypted")
		}
		if err := c.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("smtp: starttls: %w", err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("smtp: login: %w", err) // the server's reply never contains the password
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("smtp: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(s.build(from, m, time.Now())); err != nil {
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: send: %w", err)
	}
	return c.Quit()
}
