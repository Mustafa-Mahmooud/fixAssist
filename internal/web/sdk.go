// Package web holds the dashboard (Phase 3) and, for now, the SDK endpoint.
package web

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/projects"
	sdkaddon "github.com/yourname/fixassist/pkg/sdk-addon"
)

// SDK serves GET /sdk/{public_key}.js with the project's DSN baked in.
// Projects may be nil (spike capture: any key, project 1).
type SDK struct {
	Projects   ingest.ProjectLookup
	IngestBase string // public ingest URL, e.g. https://ingest.example.com
	Log        *slog.Logger
}

// Register mounts the route.
func (s *SDK) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /sdk/{file}", s.serve)
}

func (s *SDK) serve(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutSuffix(r.PathValue("file"), ".js")
	if !ok || key == "" || len(key) > 64 {
		http.NotFound(w, r)
		return
	}
	projectID := int64(1)
	if s.Projects != nil {
		p, found, err := s.Projects.Lookup(r.Context(), key)
		if err != nil {
			s.Log.Warn("sdk lookup failed", "err", err)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		projectID = p.ID
	}
	dsn, err := projects.DSN(s.IngestBase, key, projectID)
	if err != nil {
		http.Error(w, "misconfigured", http.StatusInternalServerError)
		return
	}
	body := sdkaddon.Bundle(dsn, "production")
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("ETag", etag)
	w.Header().Set("X-Fix-SDK", "sentry-"+sdkaddon.SentryVersion)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}
