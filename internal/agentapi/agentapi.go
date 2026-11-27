// Package agentapi lets the founder's coding agent (Cursor, Claude Code,
// Windsurf, VS Code, ...) work through a project's issues in a loop: get the
// evidence and fix prompt, apply the fix, report it, and check whether it
// held, attempt after attempt, until it looks fixed or needs a person.
//
// It is a remote MCP server (Streamable HTTP transport, JSON responses) at
// POST /mcp, plus the same operations as plain JSON under /agent/v1 for
// scripts. Both authenticate with a per-project agent token:
//
//	Authorization: Bearer fa_...
package agentapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/store/db"
)

// TokenPrefix starts every agent token, so leaked ones are recognizable.
const TokenPrefix = "fa_"

// NewToken returns a new token (shown to the founder once), its stored hash
// and a short display prefix.
func NewToken() (token, hash, prefix string, err error) {
	b := make([]byte, 24)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", err
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), token[:len(TokenPrefix)+6], nil
}

// HashToken is what is stored: SHA-256, hex. Tokens are random, so no salt
// or slow hash is needed.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Handler serves /mcp and /agent/v1.
type Handler struct {
	Pool  *pgxpool.Pool
	Code  ai.CodeSearcher // optional: GitHub code context for new prompts
	Model ai.LLM          // optional: our model for errors outside the catalog
	Log   *slog.Logger
	Now   func() time.Time
	// ClientIP finds the caller behind a proxy (analytics.Enricher.ClientIP,
	// which honours TRUST_PROXY); nil means the connection's address.
	ClientIP func(*http.Request) string

	pairs pairLimiter
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Register mounts the routes.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /mcp", h.mcp)
	// No server-initiated stream and no sessions: the spec's answer is 405.
	mux.HandleFunc("GET /mcp", methodNotAllowed)
	mux.HandleFunc("DELETE /mcp", methodNotAllowed)
	mux.HandleFunc("GET /agent/v1/issues", h.restList)
	mux.HandleFunc("GET /agent/v1/issues/{id}", h.restGet)
	mux.HandleFunc("POST /agent/v1/issues/{id}/fix", h.restReport)
	mux.HandleFunc("GET /agent/v1/issues/{id}/check", h.restCheck)
	// Auto-fix daemon (cmd/fixassist).
	mux.HandleFunc("POST /agent/v1/pair", h.restPair)
	mux.HandleFunc("POST /agent/v1/autofix/claim", h.restClaim)
	mux.HandleFunc("GET /agent/v1/autofix/prs", h.restPRs)
	mux.HandleFunc("POST /agent/v1/autofix/runs/{id}", h.restRunEvent)
}

func methodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "POST")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// caller is an authenticated agent token.
type caller struct {
	TokenID   int64
	ProjectID int64
	Agent     string
	Project   string
}

var errUnauthorized = errors.New("missing or invalid agent token")

func (h *Handler) auth(r *http.Request) (caller, error) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok = strings.TrimSpace(tok)
	if !ok || !strings.HasPrefix(tok, TokenPrefix) {
		return caller{}, errUnauthorized
	}
	q := db.New(h.Pool)
	row, err := q.AgentTokenByHash(r.Context(), HashToken(tok))
	if errors.Is(err, pgx.ErrNoRows) {
		return caller{}, errUnauthorized
	}
	if err != nil {
		return caller{}, err
	}
	_ = q.TouchAgentToken(r.Context(), row.ID)
	return caller{TokenID: row.ID, ProjectID: row.ProjectID, Agent: row.TargetAgent, Project: row.ProjectName}, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Handler) unauthorized(w http.ResponseWriter, err error) {
	if !errors.Is(err, errUnauthorized) {
		h.Log.Error("agent auth", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="fix-assistant", error="invalid_token"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Send a Fix Assistant agent token: Authorization: Bearer fa_... (create one on the project's Setup page)."})
}

// ctxNow lets tools share one clock per request.
func (h *Handler) ctx(r *http.Request) (context.Context, time.Time) { return r.Context(), h.now() }
