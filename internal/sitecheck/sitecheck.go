// Package sitecheck opens a founder's live site and says, in plain words,
// whether the Fix Assistant snippet is installed right. The Setup page's
// "Check my site" and the setup reminder email both use it.
package sitecheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yourname/fixassist/internal/projects"
)

// Result is the outcome of one check.
type Result struct {
	Level     string // good, warn, bad
	Title     string
	Text      string
	URL       string // what was fetched
	AllowHost string // an address the founder may want to allow
}

// Site is what's checked.
type Site struct {
	Domains    []string // the project's allowed domains; the first is fetched
	PublicKey  string
	IngestBase string // where the snippet must load from
	Builder    string // display name: "Lovable", "your AI agent"
}

// ErrNotPublic is returned when an address resolves to this server's own
// network (or a cloud metadata service) instead of the public internet.
var ErrNotPublic = errors.New("address is not on the public internet")

// PublicIP reports whether ip is a normal internet address.
func PublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "2001:db8::/32"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// IsLocalHost reports whether host is this computer or a private network.
func IsLocalHost(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && !PublicIP(ip)
}

// TestURL opens the app's first address with ?fixassist-test, which makes
// the add-on send one test error. "" when there's no public address.
func TestURL(domains []string) string {
	if len(domains) == 0 || IsLocalHost(domains[0]) {
		return ""
	}
	return "https://" + domains[0] + "/?fixassist-test"
}

// Client fetches founders' sites. The IP is checked when connecting (after
// DNS, and again for every redirect), so a name that resolves to an internal
// address can't make the server fetch its own network.
func Client() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || !PublicIP(ip) {
			return ErrNotPublic
		}
		return nil
	}}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: nil, DialContext: d.DialContext, TLSHandshakeTimeout: 5 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirect to a non-web address")
			}
			return nil
		},
	}
}

const maxBody = 1 << 20

var (
	reScriptTag = regexp.MustCompile(`(?is)<script\b([^>]*)>`)
	reSrcAttr   = regexp.MustCompile(`(?is)\bsrc\s*=\s*["']?([^"'\s>]+)`)
	reTypeAttr  = regexp.MustCompile(`(?is)\btype\s*=\s*["']?([^"'\s>]+)`)
	reSDKPath   = regexp.MustCompile(`/sdk/([0-9a-f]{32})\.js`)
	reAsyncAttr = regexp.MustCompile(`(?i)(^|\s)async(\s|=|$)`)
	reDeferAttr = regexp.MustCompile(`(?i)(^|\s)defer(\s|=|$)`)
)

// HTML looks for the project's snippet in the site's HTML.
func HTML(html, ingestBase, key, builder string) Result {
	firstCode := true // no script that runs code came before ours
	for _, m := range reScriptTag.FindAllStringSubmatch(html, -1) {
		attrs := m[1]
		src := ""
		if s := reSrcAttr.FindStringSubmatch(attrs); s != nil {
			src = s[1]
		}
		sdk := reSDKPath.FindStringSubmatch(src)
		if sdk == nil {
			typ := ""
			if t := reTypeAttr.FindStringSubmatch(attrs); t != nil {
				typ = strings.ToLower(t[1])
			}
			switch {
			case typ == "application/ld+json", typ == "application/json", typ == "importmap", typ == "speculationrules", typ == "text/template":
				continue // data, not code
			case src != "" && !reAsyncAttr.MatchString(attrs) && (typ == "module" || reDeferAttr.MatchString(attrs)):
				// Runs after the page is parsed (Vite's module script, deferred
				// scripts), so our tag still runs first wherever it is.
				continue
			}
			firstCode = false
			continue
		}
		if sdk[1] != key {
			return Result{Level: "bad", Title: "Your site has a snippet for a different app",
				Text: fmt.Sprintf("The Fix Assistant snippet on your site isn't this app's (it's from another app, or from before the key was changed). Copy the install message from Setup again and ask %s to replace the old line with it, then publish.", builder)}
		}
		if u, err := url.Parse(src); err == nil && u.Host != "" {
			if b, err := url.Parse(ingestBase); err == nil && !strings.EqualFold(u.Host, b.Host) {
				return Result{Level: "bad", Title: "The snippet points to the wrong server",
					Text: fmt.Sprintf("The snippet on your site loads from %s instead of %s. Copy the install message from Setup again and ask %s to use that exact line, then publish.", u.Host, b.Host, builder)}
			}
		}
		if !firstCode {
			return Result{Level: "warn", Title: "Installed, but other scripts load first",
				Text: fmt.Sprintf("Fix Assistant will work, but it can miss errors from the scripts above it. When you have a minute, ask %s: \"Move the Fix Assistant script tag so it is the first <script> inside <head>.\"", builder)}
		}
		return Result{Level: "good", Title: "The snippet is on your live site",
			Text: "Everything looks right. Open your site once and the first visit shows up in Fix Assistant within a few seconds."}
	}
	return Result{Level: "bad", Title: "The snippet isn't on your live site yet",
		Text: fmt.Sprintf("We opened your site and didn't find the Fix Assistant script. Usually the change just hasn't been published yet: publish your app in %s and check again. If it still isn't there, paste the install message from Setup into %s once more.", builder, builder)}
}

// Check fetches the site's first address and checks the install. A nil
// client means Client().
func Check(ctx context.Context, client *http.Client, s Site) Result {
	if len(s.Domains) == 0 {
		return Result{Level: "warn", Title: "No address to check",
			Text: "This app has no web address saved, so there's nothing to check. Open your live site once and the first visit shows up by itself."}
	}
	host := s.Domains[0]
	if IsLocalHost(host) {
		return Result{Level: "warn", Title: "This address is on your own computer",
			Text: "We can only check sites that are published on the internet. Open your app on this computer and the first visit shows up by itself."}
	}
	target := "https://" + host + "/"
	out := func(r Result) Result { r.URL = target; return r }
	if client == nil {
		client = Client()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return out(Result{Level: "bad", Title: "That address can't be opened", Text: "Check the address of your app."})
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; FixAssistSetupCheck/1.0)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := client.Do(req)
	if err != nil {
		var dnsErr *net.DNSError
		switch {
		case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
			return out(Result{Level: "bad", Title: "We couldn't find " + host,
				Text: "No website answers at this address. Check that your app is published and that the address is spelled right."})
		case errors.Is(err, ErrNotPublic):
			return out(Result{Level: "warn", Title: "This address isn't on the public internet",
				Text: "We can only check sites anyone can open. If your app is only on your own network, open it once and the first visit shows up by itself."})
		}
		return out(Result{Level: "bad", Title: "Your site didn't answer",
			Text: "We couldn't open " + host + " (it took too long or refused the connection). Check that your app is published, then try again."})
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode == 503:
		// Password protection or bot protection (Cloudflare, Vercel): the
		// site is up, we just can't look inside.
		return out(Result{Level: "warn", Title: "Your site didn't let us look",
			Text: "Your site is online but blocks automatic checks (password or bot protection). That's fine: open the site yourself and the first visit shows up by itself."})
	case resp.StatusCode == 404:
		return out(Result{Level: "bad", Title: "This address shows \"not found\"",
			Text: "Your app may not be published yet, or it lives at a different address. Publish it and check the address."})
	case resp.StatusCode >= 400:
		return out(Result{Level: "bad", Title: "Your site returned an error (" + strconv.Itoa(resp.StatusCode) + ")",
			Text: "Open " + host + " yourself to see what's wrong, then try again."})
	}
	if final := resp.Request.URL.Hostname(); !projects.OriginAllowed(s.Domains, "https://"+final) {
		// Usually a custom domain the builder forwards to: offer it.
		return out(Result{Level: "bad", Title: "Your address forwards to " + final,
			AllowHost: strings.TrimPrefix(projects.NormalizeDomain(final), "www."),
			Text:      "Visitors end up on " + final + ", which isn't one of this app's addresses, so their visits would be refused. If it's your app, allow it."})
	}
	return out(HTML(string(body), s.IngestBase, s.PublicKey, s.Builder))
}
