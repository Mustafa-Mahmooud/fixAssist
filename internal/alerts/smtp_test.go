package alerts

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP is a minimal SMTP server: it records the login, envelope and data.
type fakeSMTP struct {
	ln       net.Listener
	starttls bool
	mu       sync.Mutex
	auth     string
	from, to string
	data     string
}

func newFakeSMTP(t *testing.T, starttls bool) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, starttls: starttls}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		up := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			if f.starttls {
				say("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			} else {
				say("250-fake\r\n250 AUTH PLAIN")
			}
		case strings.HasPrefix(up, "AUTH PLAIN"):
			f.mu.Lock()
			f.auth = strings.TrimSpace(cmd[len("AUTH PLAIN"):])
			f.mu.Unlock()
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			f.mu.Lock()
			f.from = cmd[len("MAIL FROM:"):]
			f.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			f.mu.Lock()
			f.to = cmd[len("RCPT TO:"):]
			f.mu.Unlock()
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func TestSMTPSend(t *testing.T) {
	f := newFakeSMTP(t, false)
	s := &SMTP{Host: "localhost", Port: f.port(), Username: "me@example.com", Password: "app-password", From: "Fix Assistant <me@example.com>", insecure: true}
	err := s.Send(context.Background(), Message{To: "founder@example.com", Subject: "Sign in to Fix Assistant ✓", Text: "Your link:\nhttps://x/auth/magic?token=1", HTML: "<p>Your link</p>"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cred, _ := base64.StdEncoding.DecodeString(f.auth)
	if string(cred) != "\x00me@example.com\x00app-password" {
		t.Errorf("auth = %q", cred)
	}
	if f.from != "<me@example.com>" || f.to != "<founder@example.com>" {
		t.Errorf("envelope from %s to %s", f.from, f.to)
	}
	for _, want := range []string{
		"From: \"Fix Assistant\" <me@example.com>\r\n", "To: founder@example.com\r\n",
		"Subject: =?utf-8?q?Sign_in_to_Fix_Assistant_=E2=9C=93?=\r\n", "Message-ID: <", "@example.com>",
		"multipart/alternative", "Content-Type: text/plain; charset=utf-8", "Your link:\r\nhttps://x/auth/magic?token=1",
		"Content-Type: text/html; charset=utf-8", "<p>Your link</p>",
	} {
		if !strings.Contains(f.data, want) {
			t.Errorf("message missing %q:\n%s", want, f.data)
		}
	}
}

// The password is never sent over a connection that isn't encrypted.
func TestSMTPRequiresTLS(t *testing.T) {
	f := newFakeSMTP(t, false) // no STARTTLS offered
	s := &SMTP{Host: "localhost", Port: f.port(), Username: "me@example.com", Password: "secret", From: "me@example.com"}
	err := s.Send(context.Background(), Message{To: "a@example.com", Subject: "x", Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want a STARTTLS refusal", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.auth != "" {
		t.Error("the password was sent without encryption")
	}
}

func TestSMTPRejectsHeaderInjection(t *testing.T) {
	s := &SMTP{Host: "localhost", Port: 1, From: "me@example.com"}
	err := s.Send(context.Background(), Message{To: "a@example.com", Subject: "hi\r\nBcc: victim@example.com", Text: "x"})
	if err != errHeader {
		t.Errorf("err = %v, want errHeader", err)
	}
}

func TestFromEnvPicksSMTP(t *testing.T) {
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_PORT", strconv.Itoa(587))
	t.Setenv("SMTP_USERNAME", "me@example.com")
	t.Setenv("SMTP_PASSWORD", "x")
	t.Setenv("EMAIL_FROM", "")
	s, ok := FromEnv(discard()).(*SMTP)
	if !ok || s.Port != 587 || s.From != "Fix Assistant <me@example.com>" {
		t.Errorf("FromEnv = %#v", s)
	}
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
