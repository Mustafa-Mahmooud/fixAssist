package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/sitecheck"
	"github.com/yourname/fixassist/internal/store/db"
)

// Setup help for founders who aren't developers:
//
//   - the new-app form asks only for the app's address and builder;
//   - "Check my site" fetches the live site and says what's wrong with the
//     install, in plain words, instead of a silent "Waiting…";
//   - visits refused because they came from another address (usually the
//     builder's preview) are shown with an "Allow it" button;
//   - "Send a test error" opens the site with ?fixassist-test.

// editorHosts are builder addresses founders paste instead of their app's.
var editorHosts = map[string]string{
	"lovable.dev": "Lovable", "bolt.new": "Bolt", "stackblitz.com": "Bolt", "replit.com": "Replit",
	"v0.dev": "v0", "v0.app": "v0", "cursor.com": "Cursor", "github.com": "GitHub",
}

// parseAppAddress turns what a founder typed ("https://myapp.lovable.app/login",
// "myapp.com") into a host, or explains what's wrong with it.
func parseAppAddress(in string) (host, problem string) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", "Paste your app's web address, the one your visitors open (for example myapp.lovable.app)."
	}
	// "www." is dropped: an address also covers its subdomains, so
	// "myshop.com" accepts visits on both myshop.com and www.myshop.com.
	host = strings.TrimPrefix(projects.NormalizeDomain(in), "www.")
	if host == "" || strings.ContainsAny(host, " _") || len(host) > 253 {
		return "", "That doesn't look like a web address. Paste the address of your live app, for example myapp.lovable.app."
	}
	for h, builder := range editorHosts {
		if host == h {
			return "", fmt.Sprintf("That's the address of %s's editor, not of your app. Publish your app, then paste the address your visitors open (on Lovable it ends in .lovable.app).", builder)
		}
	}
	if host != "localhost" && !strings.Contains(host, ".") {
		return "", "That doesn't look like a full web address. It should look like myapp.lovable.app or myapp.com."
	}
	return host, ""
}

// hostingSuffixes are shared hosts where only the first label is the app's name.
var hostingSuffixes = []string{
	".lovable.app", ".lovableproject.com", ".bolt.host", ".netlify.app", ".vercel.app", ".replit.app",
	".repl.co", ".pages.dev", ".web.app", ".firebaseapp.com", ".onrender.com", ".fly.dev", ".github.io",
}

// nameFromHost is the default app name: "myapp.lovable.app" -> "myapp",
// "www.mybooking.com" -> "mybooking.com".
func nameFromHost(host string) string {
	host = strings.TrimPrefix(host, "www.")
	for _, s := range hostingSuffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return strings.TrimSuffix(host, s)
		}
	}
	return host
}

// --- live status extras ---------------------------------------------------

// RejectedSite is an address whose visits were refused.
type RejectedSite struct {
	Host     string
	Hits     int64
	LastSeen time.Time
}

// setupExtras fills what the live status box needs besides the counts.
func (a *App) setupExtras(ctx context.Context, p db.GetOrgProjectRow, st *setupStatus) error {
	q := db.New(a.Pool)
	id, err := q.FirstIssueID(ctx, p.ID)
	if err != nil {
		return err
	}
	st.FirstIssueID = id
	st.TestURL = testURL(p.AllowedDomains)
	if a.GitHub != nil && st.Connected {
		// Right after the first visit is when code context pays off next:
		// offer it here instead of only under Optional extras.
		g, err := q.GetProjectGitHub(ctx, p.ID)
		st.OfferGitHub = errors.Is(err, pgx.ErrNoRows) || (err == nil && !g.RepoFullName.Valid && g.InstallationID == 0)
	}
	if len(p.AllowedDomains) == 0 {
		return nil // every site is allowed: nothing is refused
	}
	rows, err := q.ListRejectedOrigins(ctx, p.ID)
	if err != nil {
		return err
	}
	for _, r := range rows {
		// Ingest caches domains for up to 30 seconds, so a just-allowed
		// host can be recorded once more: hide it.
		if projects.OriginAllowed(p.AllowedDomains, "https://"+r.Host) {
			continue
		}
		st.Rejected = append(st.Rejected, RejectedSite{Host: r.Host, Hits: r.Hits, LastSeen: r.LastSeenAt.Time})
	}
	return nil
}

// testURL opens the app's first address with ?fixassist-test.
func testURL(domains []string) string { return sitecheck.TestURL(domains) }

// allowDomain adds an address to the project's allowed domains.
func (a *App) allowDomain(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	host, problem := parseAppAddress(r.PostFormValue("host"))
	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	q := db.New(a.Pool)
	if err := q.AddAllowedDomain(r.Context(), db.AddAllowedDomainParams{ID: p.ID, Domain: host}); err != nil {
		a.fail(w, err)
		return
	}
	if err := q.DeleteRejectedOrigin(r.Context(), db.DeleteRejectedOriginParams{ProjectID: p.ID, Host: host}); err != nil {
		a.fail(w, err)
		return
	}
	http.Redirect(w, r, "/projects/"+strconv.FormatInt(p.ID, 10)+"/setup?allowed="+url.QueryEscape(host)+"#install-status", http.StatusSeeOther)
}

// --- Check my site --------------------------------------------------------

// SiteCheck is a check result plus what the "Allow" form needs.
type SiteCheck struct {
	sitecheck.Result
	ProjectID int64
	CSRF      string
}

// checkSite fetches the project's first address and checks the install.
func (a *App) checkSite(ctx context.Context, p db.GetOrgProjectRow) SiteCheck {
	return SiteCheck{Result: sitecheck.Check(ctx, a.SiteClient, sitecheck.Site{
		Domains: p.AllowedDomains, PublicKey: p.PublicKey, IngestBase: a.IngestBase, Builder: agentName(p.TargetAgent),
	})}
}

// siteCheckEvery limits how often one project can be checked.
const siteCheckEvery = 5 * time.Second

func (a *App) siteCheck(w http.ResponseWriter, r *http.Request) {
	p, ok := a.project(w, r)
	if !ok {
		return
	}
	var c SiteCheck
	if a.siteChecks.allow(p.ID, time.Now()) {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		c = a.checkSite(ctx, p)
	} else {
		c = SiteCheck{Result: sitecheck.Result{Level: "warn", Title: "Just a moment", Text: "You checked a few seconds ago. Try again in a moment."}}
	}
	c.ProjectID, c.CSRF = p.ID, sessionFrom(r).CSRF
	a.partial(w, "site_check", c)
}

// projectThrottle lets each project through at most once per siteCheckEvery.
type projectThrottle struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

func (t *projectThrottle) allow(id int64, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil || len(t.last) > 10_000 {
		t.last = map[int64]time.Time{}
	}
	if l, ok := t.last[id]; ok && now.Sub(l) < siteCheckEvery {
		return false
	}
	t.last[id] = now
	return true
}
