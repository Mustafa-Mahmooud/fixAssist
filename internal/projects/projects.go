// Package projects owns orgs, projects, DSN keys and quotas.
package projects

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/store/db"
)

// Project is what ingest needs to know about a DSN key's project.
type Project struct {
	ID             int64
	PublicKey      string
	TargetAgent    string
	Backend        string
	AllowedDomains []string
	QuotaMonthly   int64
	HMACSecret     []byte
}

// Agents and backends accepted by the schema.
var (
	Agents   = []string{"lovable", "bolt", "cursor", "replit", "v0", "other"}
	Backends = []string{"supabase", "firebase", "unknown"}
)

// DefaultQuotaMonthly is the design-partner monthly event quota.
const DefaultQuotaMonthly = 500_000

// Service creates and changes projects.
type Service struct {
	Pool *pgxpool.Pool
}

// CreateParams describes a new project.
type CreateParams struct {
	OrgID          int64 // existing org; if 0, Org is looked up or created by name
	Org            string
	Name           string
	Agent          string
	Backend        string
	AllowedDomains []string
	QuotaMonthly   int64
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// NewPublicKey returns a 32-hex-character DSN public key.
func NewPublicKey() string { return randomHex(16) }

func oneOf(v string, allowed []string) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// Create makes the org (if new), the project and its first key in one transaction.
func (s *Service) Create(ctx context.Context, p CreateParams) (Project, error) {
	if p.Name == "" || (p.Org == "" && p.OrgID == 0) {
		return Project{}, errors.New("org and name are required")
	}
	if p.Agent == "" {
		p.Agent = "other"
	}
	if p.Backend == "" {
		p.Backend = "unknown"
	}
	if !oneOf(p.Agent, Agents) {
		return Project{}, fmt.Errorf("agent must be one of %v", Agents)
	}
	if !oneOf(p.Backend, Backends) {
		return Project{}, fmt.Errorf("backend must be one of %v", Backends)
	}
	if p.QuotaMonthly <= 0 {
		p.QuotaMonthly = DefaultQuotaMonthly
	}
	domains := make([]string, 0, len(p.AllowedDomains))
	for _, d := range p.AllowedDomains {
		if d = NormalizeDomain(d); d != "" {
			domains = append(domains, d)
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return Project{}, err
	}

	var out Project
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		orgID := p.OrgID
		if orgID == 0 {
			var err error
			orgID, err = q.GetOrgByName(ctx, p.Org)
			if errors.Is(err, pgx.ErrNoRows) {
				orgID, err = q.CreateOrg(ctx, p.Org)
			}
			if err != nil {
				return err
			}
		}
		row, err := q.CreateProject(ctx, db.CreateProjectParams{
			OrgID: orgID, Name: p.Name, TargetAgent: p.Agent, Backend: p.Backend,
			StackTags: []string{}, AllowedDomains: domains, QuotaMonthly: p.QuotaMonthly, HmacSecret: secret,
		})
		if err != nil {
			return err
		}
		key := NewPublicKey()
		if err := q.CreateProjectKey(ctx, db.CreateProjectKeyParams{PublicKey: key, ProjectID: row.ID}); err != nil {
			return err
		}
		out = Project{ID: row.ID, PublicKey: key, TargetAgent: p.Agent, Backend: p.Backend,
			AllowedDomains: domains, QuotaMonthly: p.QuotaMonthly, HMACSecret: secret}
		return nil
	})
	return out, err
}

// RotateKey revokes every active key of the project and issues a new one.
// Caches drop revoked keys on their next refresh (at most 30 s).
func (s *Service) RotateKey(ctx context.Context, projectID int64) (string, error) {
	key := NewPublicKey()
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.RevokeProjectKeys(ctx, projectID)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("project %d has no active key (does it exist?)", projectID)
		}
		return q.CreateProjectKey(ctx, db.CreateProjectKeyParams{PublicKey: key, ProjectID: projectID})
	})
	return key, err
}

// SetQuota changes the monthly event quota.
func (s *Service) SetQuota(ctx context.Context, projectID, quota int64) error {
	return db.New(s.Pool).SetProjectQuota(ctx, db.SetProjectQuotaParams{ID: projectID, QuotaMonthly: quota})
}

// List returns all projects with their current key.
func (s *Service) List(ctx context.Context) ([]db.ListProjectsRow, error) {
	return db.New(s.Pool).ListProjects(ctx)
}

// NormalizeDomain lowercases and strips scheme, port and path:
// "https://App.Example.com:443/x" -> "app.example.com".
func NormalizeDomain(d string) string {
	d = strings.TrimSpace(strings.ToLower(d))
	if d == "" {
		return ""
	}
	if !strings.Contains(d, "://") {
		d = "http://" + d
	}
	u, err := url.Parse(d)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// OriginAllowed reports whether an Origin header is allowed by the list.
// An empty list allows everything; an entry also allows its subdomains.
func OriginAllowed(allowed []string, origin string) bool {
	if len(allowed) == 0 {
		return true
	}
	host := NormalizeDomain(origin)
	if host == "" {
		return false
	}
	for _, a := range allowed {
		if host == a || strings.HasSuffix(host, "."+a) {
			return true
		}
	}
	return false
}

// DSN builds the Sentry DSN for a key: <scheme>://<key>@<host>/<project_id>.
func DSN(ingestBase string, key string, projectID int64) (string, error) {
	u, err := url.Parse(strings.TrimRight(ingestBase, "/"))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("bad ingest URL %q", ingestBase)
	}
	return fmt.Sprintf("%s://%s@%s%s/%d", u.Scheme, key, u.Host, u.Path, projectID), nil
}
