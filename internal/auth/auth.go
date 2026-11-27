// Package auth implements magic-link sign-in and browser sessions.
// GitHub OAuth (FDR) comes later; repo access is a separate GitHub App.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/alerts"
	"github.com/yourname/fixassist/internal/store/db"
)

// FDR auth parameters.
const (
	TokenTTL        = 15 * time.Minute
	SessionTTL      = 30 * 24 * time.Hour
	MaxLinksPerHour = 5
	// Sessions are extended when less than this much of their life is left
	// unused, so active users stay signed in (sliding expiry) without a write
	// on every request.
	extendAfter = 24 * time.Hour
	touchEvery  = 15 * time.Minute // how often last_seen_at is refreshed
)

// Errors returned to the web layer.
var (
	ErrBadEmail    = errors.New("please enter a valid email address")
	ErrRateLimited = errors.New("too many sign-in links requested; try again in an hour")
	ErrBadToken    = errors.New("this sign-in link is invalid, expired or already used")
)

// Service issues links and sessions.
type Service struct {
	Pool    *pgxpool.Pool
	Sender  alerts.Sender
	BaseURL string // public URL of the dashboard, for links
	Now     func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Session is a signed-in browser.
type Session struct {
	UserID  int64
	Email   string
	CSRF    string
	OrgID   int64
	OrgName string
	// Staff: role 'admin', or (set by the web layer) listed in STAFF_EMAILS.
	Staff bool
	// Owner: listed in STAFF_EMAILS, so always admin; set by the web layer.
	Owner bool
	// StaffView: the other account's name while an admin works on its
	// project (full access); set by the web layer, empty otherwise.
	StaffView string
	// TipsHidden: the person turned off the "what this page shows" notes.
	TipsHidden bool
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) []byte {
	sum := sha256.Sum256([]byte(t))
	return sum[:]
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// NormalizeEmail validates and lowercases an address.
func NormalizeEmail(raw string) (string, error) {
	a, err := mail.ParseAddress(strings.TrimSpace(raw))
	if err != nil || !strings.Contains(a.Address, ".") || len(a.Address) > 254 {
		return "", ErrBadEmail
	}
	return strings.ToLower(a.Address), nil
}

// RequestLink emails a single-use sign-in link (sign-up and sign-in are the
// same flow; the account is created when the link is used).
func (s *Service) RequestLink(ctx context.Context, rawEmail string) error {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return err
	}
	q := db.New(s.Pool)
	n, err := q.CountRecentLoginTokens(ctx, db.CountRecentLoginTokensParams{Lower: email, CreatedAt: ts(s.now().Add(-time.Hour))})
	if err != nil {
		return err
	}
	if n >= MaxLinksPerHour {
		return ErrRateLimited
	}
	link, err := s.issue(ctx, email, TokenTTL)
	if err != nil {
		return err
	}
	return s.Sender.Send(ctx, alerts.Message{
		To:      email,
		Subject: "Your Fix Assistant sign-in link",
		Text: fmt.Sprintf("Click to sign in to Fix Assistant:\n\n%s\n\nThe link works once and expires in %d minutes. If you didn't ask for it, ignore this email.\n",
			link, int(TokenTTL.Minutes())),
		HTML: alerts.RenderHTML(alerts.Email{
			Preheader: fmt.Sprintf("Your sign-in link. It works once and expires in %d minutes.", int(TokenTTL.Minutes())),
			Heading:   "Sign in to Fix Assistant",
			Paras:     []string{"Click the button below to sign in. No password needed."},
			Buttons:   []alerts.Button{{Label: "Sign in", URL: link}},
			Note: fmt.Sprintf("The link works once and expires in %d minutes. If you didn't ask for it, you can safely ignore this email.",
				int(TokenTTL.Minutes())),
			Footer: "Sent to " + email + " because someone asked to sign in with this address.",
		}),
	})
}

func (s *Service) issue(ctx context.Context, email string, ttl time.Duration) (string, error) {
	tok := randomToken()
	if err := db.New(s.Pool).InsertLoginToken(ctx, db.InsertLoginTokenParams{TokenHash: hashToken(tok), Email: email, ExpiresAt: ts(s.now().Add(ttl))}); err != nil {
		return "", err
	}
	return strings.TrimRight(s.BaseURL, "/") + "/auth/magic?token=" + tok, nil
}

// AdminLinkTTL is how long an operator-issued link stays valid: long enough
// to hand to a partner by chat while email isn't set up.
const AdminLinkTTL = 72 * time.Hour

// AdminLink issues a single-use sign-in link without sending email, for
// operators to deliver by hand (`app admin login-link`). Not rate limited.
func (s *Service) AdminLink(ctx context.Context, rawEmail string) (string, error) {
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return "", err
	}
	return s.issue(ctx, email, AdminLinkTTL)
}

// UseLink consumes a token and returns the user, creating the account (and
// its org, FDR: one org per user at signup) on first sign-in.
func (s *Service) UseLink(ctx context.Context, token string) (int64, error) {
	if token == "" || len(token) > 100 {
		return 0, ErrBadToken
	}
	var userID int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		email, err := q.UseLoginToken(ctx, hashToken(token))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrBadToken
		}
		if err != nil {
			return err
		}
		u, err := q.GetUserByEmail(ctx, email)
		if err == nil {
			userID = u.ID
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		nu, err := q.CreateUser(ctx, email)
		if err != nil {
			return err
		}
		orgID, err := q.CreateOrg(ctx, email+"'s apps")
		if err != nil {
			return err
		}
		userID = nu.ID
		return q.CreateMembership(ctx, db.CreateMembershipParams{UserID: nu.ID, OrgID: orgID, Role: "owner"})
	})
	return userID, err
}

// ErrGitHubConflict: the email's account is already linked to another
// GitHub user, so this one can't take it over.
var ErrGitHubConflict = errors.New("this email's account is linked to a different GitHub account; sign in with your email link instead")

// SignInGitHub returns the account for a GitHub user, by GitHub id first,
// then by the verified email GitHub returned (linking the two), else a new
// account and org, as a first email sign-in would create.
func (s *Service) SignInGitHub(ctx context.Context, githubID int64, verifiedEmail string) (int64, error) {
	email := strings.ToLower(strings.TrimSpace(verifiedEmail))
	if githubID == 0 || !strings.Contains(email, "@") {
		return 0, errors.New("github sign-in: missing id or email")
	}
	var userID int64
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if u, err := q.GetUserByGitHubID(ctx, pgtype.Int8{Int64: githubID, Valid: true}); err == nil {
			userID = u.ID
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if u, err := q.GetUserByEmail(ctx, email); err == nil {
			n, err := q.LinkGitHub(ctx, db.LinkGitHubParams{ID: u.ID, GithubID: pgtype.Int8{Int64: githubID, Valid: true}})
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrGitHubConflict
			}
			userID = u.ID
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		nu, err := q.CreateUserWithGitHub(ctx, db.CreateUserWithGitHubParams{Email: email, GithubID: pgtype.Int8{Int64: githubID, Valid: true}})
		if err != nil {
			return err
		}
		orgID, err := q.CreateOrg(ctx, email+"'s apps")
		if err != nil {
			return err
		}
		userID = nu.ID
		return q.CreateMembership(ctx, db.CreateMembershipParams{UserID: nu.ID, OrgID: orgID, Role: "owner"})
	})
	return userID, err
}

// CreateSession returns the cookie value for a new session.
func (s *Service) CreateSession(ctx context.Context, userID int64) (string, error) {
	cookie := randomToken()
	err := db.New(s.Pool).CreateWebSession(ctx, db.CreateWebSessionParams{
		IDHash: hashToken(cookie), UserID: userID, CsrfToken: randomToken(), ExpiresAt: ts(s.now().Add(SessionTTL)),
	})
	return cookie, err
}

// LoadSession resolves a cookie value; ok is false when signed out.
func (s *Service) LoadSession(ctx context.Context, cookie string) (Session, bool, error) {
	if cookie == "" || len(cookie) > 100 {
		return Session{}, false, nil
	}
	q := db.New(s.Pool)
	id := hashToken(cookie)
	row, err := q.GetWebSession(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, err
	}
	if row.ExpiresAt.Time.Sub(s.now()) < SessionTTL-extendAfter {
		if err := q.ExtendWebSession(ctx, db.ExtendWebSessionParams{IDHash: id, ExpiresAt: ts(s.now().Add(SessionTTL))}); err != nil {
			return Session{}, false, err
		}
	}
	// Activity for admin analytics, at most every 15 minutes per person. A
	// failed write only costs a data point, so it never fails the request.
	if !row.LastSeenAt.Valid || s.now().Sub(row.LastSeenAt.Time) > touchEvery {
		_ = q.TouchUser(ctx, row.UserID)
	}
	org, err := q.FirstOrgForUser(ctx, row.UserID)
	if err != nil {
		return Session{}, false, fmt.Errorf("user %d has no org: %w", row.UserID, err)
	}
	return Session{UserID: row.UserID, Email: row.Email, CSRF: row.CsrfToken, OrgID: org.ID, OrgName: org.Name, TipsHidden: row.TipsHidden,
		Staff: row.Role == "admin"}, true, nil
}

// EndSession signs a browser out.
func (s *Service) EndSession(ctx context.Context, cookie string) error {
	if cookie == "" {
		return nil
	}
	return db.New(s.Pool).DeleteWebSession(ctx, hashToken(cookie))
}

// CSRFValid compares in constant time.
func CSRFValid(sess Session, got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(sess.CSRF), []byte(got)) == 1
}
