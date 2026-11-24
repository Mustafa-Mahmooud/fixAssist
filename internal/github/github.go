// Package github is the GitHub App for code context (FDR Phase 7): founders
// install it on their repo (read-only Contents), and fix prompts can then
// name the exact file and line. Code is scanned in memory; only file paths,
// line numbers and short snippets are kept.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// App holds the GitHub App credentials (from the manifest conversion).
type App struct {
	ID           int64
	Slug         string
	ClientID     string
	ClientSecret string
	Key          *rsa.PrivateKey
	APIBase      string // https://api.github.com (tests point this at a fake)
	WebBase      string // https://github.com
	HTTP         *http.Client

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

// FromEnv loads GITHUB_APP_ID, GITHUB_APP_SLUG, GITHUB_CLIENT_ID,
// GITHUB_CLIENT_SECRET and GITHUB_APP_PRIVATE_KEY (PEM text or a file path).
// It returns nil when the app isn't configured.
func FromEnv() (*App, error) {
	id := os.Getenv("GITHUB_APP_ID")
	if id == "" {
		return nil, nil
	}
	appID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("GITHUB_APP_ID: %w", err)
	}
	keyPEM := os.Getenv("GITHUB_APP_PRIVATE_KEY")
	if !strings.Contains(keyPEM, "BEGIN") {
		b, err := os.ReadFile(keyPEM)
		if err != nil {
			return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY: %w", err)
		}
		keyPEM = string(b)
	}
	key, err := ParseKey([]byte(keyPEM))
	if err != nil {
		return nil, err
	}
	return &App{ID: appID, Slug: os.Getenv("GITHUB_APP_SLUG"), ClientID: os.Getenv("GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"), Key: key}, nil
}

// ParseKey reads the app's PEM private key (PKCS#1 or PKCS#8).
func ParseKey(p []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(p)
	if block == nil {
		return nil, errors.New("github app key: not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app key: not RSA")
	}
	return rk, nil
}

func (a *App) api() string {
	if a.APIBase != "" {
		return strings.TrimRight(a.APIBase, "/")
	}
	return "https://api.github.com"
}

func (a *App) web() string {
	if a.WebBase != "" {
		return strings.TrimRight(a.WebBase, "/")
	}
	return "https://github.com"
}

func (a *App) client() *http.Client {
	if a.HTTP != nil {
		return a.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// JWT signs the short-lived app token (RS256, iss = app id, 9 minutes).
func (a *App) JWT(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(a.ID, 10)})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

func (a *App) do(ctx context.Context, method, url, auth string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("github %s %s: %d %s", method, url, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// InstallationToken returns a cached installation access token.
func (a *App) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	a.mu.Lock()
	if t, ok := a.tokens[installationID]; ok && time.Until(t.expires) > time.Minute {
		a.mu.Unlock()
		return t.token, nil
	}
	a.mu.Unlock()
	jwt, err := a.JWT(time.Now())
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.do(ctx, "POST", fmt.Sprintf("%s/app/installations/%d/access_tokens", a.api(), installationID), "Bearer "+jwt, nil, &out); err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.tokens == nil {
		a.tokens = map[int64]cachedToken{}
	}
	a.tokens[installationID] = cachedToken{out.Token, out.ExpiresAt}
	a.mu.Unlock()
	return out.Token, nil
}

// InstallURL starts the install on github.com; state comes back to setup.
func (a *App) InstallURL(state string) string {
	return fmt.Sprintf("%s/apps/%s/installations/new?state=%s", a.web(), url.PathEscape(a.Slug), url.QueryEscape(state))
}

// UserCanAccess exchanges the OAuth code GitHub sends with the install
// redirect and checks that this GitHub user can access the installation.
// Without this, anyone could attach someone else's installation_id.
func (a *App) UserCanAccess(ctx context.Context, code string, installationID int64) (bool, error) {
	form := url.Values{"client_id": {a.ClientID}, "client_secret": {a.ClientSecret}, "code": {code}}
	req, _ := http.NewRequestWithContext(ctx, "POST", a.web()+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return false, err
	}
	if tok.AccessToken == "" {
		return false, fmt.Errorf("github oauth: %s", tok.Error)
	}
	var out struct {
		Installations []struct {
			ID int64 `json:"id"`
		} `json:"installations"`
	}
	if err := a.do(ctx, "GET", a.api()+"/user/installations?per_page=100", "Bearer "+tok.AccessToken, nil, &out); err != nil {
		return false, err
	}
	for _, in := range out.Installations {
		if in.ID == installationID {
			return true, nil
		}
	}
	return false, nil
}

// Repos lists the repositories an installation can read.
func (a *App) Repos(ctx context.Context, installationID int64) ([]string, error) {
	tok, err := a.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := a.do(ctx, "GET", a.api()+"/installation/repositories?per_page=100", "Bearer "+tok, nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Repositories))
	for _, r := range out.Repositories {
		names = append(names, r.FullName)
	}
	return names, nil
}

// Tarball downloads the default branch as a gzipped tarball (max 50 MB).
func (a *App) Tarball(ctx context.Context, installationID int64, repo string) ([]byte, error) {
	tok, err := a.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", a.api()+"/repos/"+repo+"/tarball", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.client().Do(req) // follows the redirect to codeload
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github tarball %s: %d", repo, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, MaxTarball))
}

// MaxTarball caps the repo download.
const MaxTarball = 50 << 20

// Manifest is the GitHub App manifest: read-only code access, and GitHub
// asks the user to sign in during install so we can verify ownership.
func Manifest(baseURL string) []byte {
	base := strings.TrimRight(baseURL, "/")
	host := strings.TrimPrefix(strings.TrimPrefix(base, "https://"), "http://")
	m := map[string]any{
		"name":                     "Fix Assistant (" + host + ")",
		"url":                      base,
		"redirect_url":             base + "/admin/github/callback",
		"callback_urls":            []string{base + "/github/setup"},
		"setup_url":                base + "/github/setup",
		"request_oauth_on_install": true,
		"public":                   true,
		"hook_attributes":          map[string]any{"url": base + "/github/webhook", "active": false},
		"default_permissions":      map[string]string{"contents": "read", "metadata": "read"},
		"default_events":           []string{},
	}
	b, _ := json.Marshal(m)
	return b
}

// Conversion is what GitHub returns after the manifest flow.
type Conversion struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	HTMLURL       string `json:"html_url"`
}

// Convert finishes the manifest flow (no auth needed; the code is single-use).
func Convert(ctx context.Context, apiBase, code string) (Conversion, error) {
	a := &App{APIBase: apiBase}
	var c Conversion
	err := a.do(ctx, "POST", a.api()+"/app-manifests/"+url.PathEscape(code)+"/conversions", "", bytes.NewReader(nil), &c)
	return c, err
}
