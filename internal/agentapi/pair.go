package agentapi

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/store/db"
)

// Pairing: the Setup page shows a one-time code inside the auto-fix install
// command, and fixassist trades it for an agent token here. The founder
// never sees or copies a token.
//
//	POST /agent/v1/pair {"code": "K7QM4-X2PRA", "hostname": "laptop"}
//	-> {"token": "fa_...", "project_id": 1, "project": "myshop"}

// PairCodeTTL is how long a code works.
const PairCodeTTL = 15 * time.Minute

// Codes avoid look-alike characters (0/O, 1/I/L): they are read off a
// screen. 10 characters of 31 is about 49 bits, for a code that lives 15
// minutes and works once.
const pairAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// NewPairCode returns a code to show ("K7QM4-X2PRA") and its stored hash.
func NewPairCode() (code, hash string, err error) {
	var b strings.Builder
	for i := 0; i < 10; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pairAlphabet))))
		if err != nil {
			return "", "", err
		}
		if i == 5 {
			b.WriteByte('-')
		}
		b.WriteByte(pairAlphabet[n.Int64()])
	}
	code = b.String()
	return code, HashPairCode(code), nil
}

// NormalizePairCode accepts what people type: lower case, spaces, dashes.
func NormalizePairCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(pairAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// HashPairCode is what is stored.
func HashPairCode(code string) string { return HashToken("pair:" + NormalizePairCode(code)) }

// CreatePairCode stores a new code for the project.
func CreatePairCode(ctx context.Context, pool *pgxpool.Pool, projectID, userID int64, now time.Time) (string, error) {
	code, hash, err := NewPairCode()
	if err != nil {
		return "", err
	}
	err = db.New(pool).CreatePairCode(ctx, db.CreatePairCodeParams{
		CodeHash: hash, ProjectID: projectID, CreatedBy: pgtype.Int8{Int64: userID, Valid: userID != 0},
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(PairCodeTTL), Valid: true},
	})
	return code, err
}

var errBadCode = errors.New("this code is wrong, expired or already used: get a new one on the Setup page")

// PairResult is what fixassist gets back.
type PairResult struct {
	Token     string `json:"token"`
	ProjectID int64  `json:"project_id"`
	Project   string `json:"project"`
}

// Pair trades a code for a new agent token, once.
func Pair(ctx context.Context, pool *pgxpool.Pool, code, hostname string) (PairResult, error) {
	if len(NormalizePairCode(code)) != 10 {
		return PairResult{}, errBadCode
	}
	var out PairResult
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		hash := HashPairCode(code)
		row, err := q.UsePairCode(ctx, hash)
		if errors.Is(err, pgx.ErrNoRows) {
			return errBadCode
		}
		if err != nil {
			return err
		}
		token, tokenHash, prefix, err := NewToken()
		if err != nil {
			return err
		}
		t, err := q.CreateAgentToken(ctx, db.CreateAgentTokenParams{
			ProjectID: row.ProjectID, Name: tokenName(hostname), TokenHash: tokenHash, Prefix: prefix, CreatedBy: row.CreatedBy,
		})
		if err != nil {
			return err
		}
		if err := q.SetPairCodeToken(ctx, db.SetPairCodeTokenParams{CodeHash: hash, TokenID: pgtype.Int8{Int64: t.ID, Valid: true}}); err != nil {
			return err
		}
		name, err := q.ProjectName(ctx, row.ProjectID)
		if err != nil {
			return err
		}
		out = PairResult{Token: token, ProjectID: row.ProjectID, Project: name}
		return nil
	})
	return out, err
}

// tokenName: "fixassist on laptop", printable and short.
func tokenName(hostname string) string {
	h := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) && r != '<' && r != '>' {
			return r
		}
		return -1
	}, strings.TrimSpace(hostname))
	if h == "" {
		h = "a computer"
	}
	if len(h) > 40 {
		h = h[:40]
	}
	return "fixassist on " + h
}

func (h *Handler) restPair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code     string `json:"code"`
		Hostname string `json:"hostname"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	if !h.pairs.allow(h.clientKey(r), h.now()) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many tries, wait a minute"})
		return
	}
	res, err := Pair(r.Context(), h.Pool, in.Code, in.Hostname)
	if errors.Is(err, errBadCode) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		h.Log.Error("pair", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// pairLimiter allows a few redeem attempts per client a minute, so codes
// can't be guessed online.
type pairLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

const pairTriesPerMinute = 10

func (l *pairLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil || len(l.hits) > 10_000 {
		l.hits = map[string][]time.Time{}
	}
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= pairTriesPerMinute {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}

func (h *Handler) clientKey(r *http.Request) string {
	if h.ClientIP != nil {
		return h.ClientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
