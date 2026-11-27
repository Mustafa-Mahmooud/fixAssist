package ingest

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store/db"
)

// Setup signals: small facts ingest learns that help a founder finish
// setup without asking them technical questions.
//
//   - a refused Origin (the site isn't in the allowed domains, e.g. the
//     builder's preview address) is recorded, so Setup can offer "allow it";
//   - the add-on reports which backend the app talks to (Supabase or
//     Firebase), so the new-app form doesn't have to ask.

// SetupSignals is optional on a Sink; PGSink implements it.
type SetupSignals interface {
	RejectedOrigin(ctx context.Context, projectID int64, host string) error
	DetectedBackend(ctx context.Context, projectID int64, backend string) error
}

// signalThrottle lets each (project, signal) through at most once per
// period, so a busy site with a wrong domain costs one write a minute.
type signalThrottle struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const signalPeriod = time.Minute

func (t *signalThrottle) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil || len(t.seen) > 10_000 {
		t.seen = map[string]time.Time{}
	}
	if last, ok := t.seen[key]; ok && now.Sub(last) < signalPeriod {
		return false
	}
	t.seen[key] = now
	return true
}

// noteRejectedOrigin records a refused browser origin in the background.
func (h *Handler) noteRejectedOrigin(p projects.Project, origin string) {
	ss, ok := h.Sink.(SetupSignals)
	if !ok || p.ID == 0 || origin == "" || origin == "null" {
		return
	}
	host := projects.NormalizeDomain(origin)
	if host == "" || len(host) > 253 {
		return
	}
	if !h.signals.allow("origin|"+strconv.FormatInt(p.ID, 10)+"|"+host, time.Now()) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := ss.RejectedOrigin(ctx, p.ID, host); err != nil {
			h.Log.Warn("record rejected origin", "project_id", p.ID, "err", err)
		}
	}()
}

// Backends the add-on can detect from the requests the app makes.
var detectableBackends = map[string]bool{"supabase": true, "firebase": true}

// noteBackend fills in the project's backend when it is still unknown.
func (h *Handler) noteBackend(ctx context.Context, p projects.Project, backend string) {
	ss, ok := h.Sink.(SetupSignals)
	if !ok || p.ID == 0 || p.Backend != "unknown" || !detectableBackends[backend] {
		return
	}
	if !h.signals.allow("backend|"+strconv.FormatInt(p.ID, 10), time.Now()) {
		return
	}
	if err := ss.DetectedBackend(ctx, p.ID, backend); err != nil {
		h.Log.Warn("set detected backend", "project_id", p.ID, "err", err)
	}
}

func (s *PGSink) RejectedOrigin(ctx context.Context, projectID int64, host string) error {
	return db.New(s.Pool).RecordRejectedOrigin(ctx, db.RecordRejectedOriginParams{ProjectID: projectID, Host: host})
}

func (s *PGSink) DetectedBackend(ctx context.Context, projectID int64, backend string) error {
	_, err := db.New(s.Pool).SetDetectedBackend(ctx, db.SetDetectedBackendParams{ID: projectID, Backend: backend})
	return err
}
