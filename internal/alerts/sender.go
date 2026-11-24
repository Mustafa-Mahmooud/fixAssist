// Package alerts sends email: magic links now, instant alerts and digests in
// Phase 4. Everything goes through Sender, so providers are swappable.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Message is one email.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Sender delivers email.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Off reports whether email is switched off (EMAIL_MODE=off): sign-in links
// are then issued by operators with "app admin login-link".
func Off() bool { return os.Getenv("EMAIL_MODE") == "off" && os.Getenv("RESEND_API_KEY") == "" }

// FromEnv picks Resend when RESEND_API_KEY is set, else the dev sender.
func FromEnv(log *slog.Logger) Sender {
	if key := os.Getenv("RESEND_API_KEY"); key != "" {
		from := os.Getenv("EMAIL_FROM")
		if from == "" {
			from = "Fix Assistant <onboarding@resend.dev>"
		}
		return &Resend{APIKey: key, From: from}
	}
	dir := os.Getenv("EMAIL_DEV_DIR")
	if dir == "" {
		dir = filepath.Join("tmp", "mail")
	}
	return &Dev{Dir: dir, Log: log}
}

// Dev writes each email to Dir as an .eml file and logs its subject and links.
// Never use it in production: the log contains login links.
type Dev struct {
	Dir string
	Log *slog.Logger
}

var reLink = regexp.MustCompile(`https?://\S+`)
var reUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (d *Dev) Send(_ context.Context, m Message) error {
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%d-%s.eml", time.Now().UnixNano(), reUnsafe.ReplaceAllString(m.To, "_"))
	var b strings.Builder
	fmt.Fprintf(&b, "To: %s\r\nSubject: %s\r\nDate: %s\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
		m.To, m.Subject, time.Now().Format(time.RFC1123Z), m.Text)
	if err := os.WriteFile(filepath.Join(d.Dir, name), []byte(b.String()), 0o600); err != nil {
		return err
	}
	d.Log.Info("dev email (not sent)", "to", m.To, "subject", m.Subject, "file", name, "links", reLink.FindAllString(m.Text, -1))
	return nil
}

// Resend sends through the Resend HTTP API.
type Resend struct {
	APIKey string
	From   string
	Client *http.Client
}

func (r *Resend) Send(ctx context.Context, m Message) error {
	body, _ := json.Marshal(map[string]any{"from": r.From, "to": []string{m.To}, "subject": m.Subject, "text": m.Text, "html": m.HTML})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	req.Header.Set("Content-Type", "application/json")
	c := r.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("resend: status %d", resp.StatusCode)
	}
	return nil
}
