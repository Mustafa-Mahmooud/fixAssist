package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// FileSink writes every accepted payload to a directory. It is used by the
// Phase 1 spike to capture golden files from the real SDK.
type FileSink struct {
	Dir string
	Log *slog.Logger
	seq atomic.Int64
	mu  sync.Mutex
}

func (s *FileSink) name(kind string) string {
	n := s.seq.Add(1)
	return filepath.Join(s.Dir, fmt.Sprintf("%d-%04d.%s", time.Now().UnixMilli(), n, kind))
}

// Envelope writes the decompressed envelope and a small meta sidecar.
func (s *FileSink) Envelope(_ context.Context, rec Received) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.name("envelope")
	if err := os.WriteFile(p, rec.Raw, 0o644); err != nil {
		return err
	}
	types := make([]string, 0, len(rec.Envelope.Items))
	for _, it := range rec.Envelope.Items {
		types = append(types, it.Header.Type)
	}
	meta, _ := json.MarshalIndent(map[string]any{
		"project_id": rec.ProjectID,
		"origin":     rec.Origin,
		"items":      types,
		"events":     len(rec.Events),
	}, "", "  ")
	s.Log.Info("captured envelope", "file", filepath.Base(p), "items", types)
	return os.WriteFile(p+".meta.json", meta, 0o644)
}

// Pageview writes the page-view JSON body.
func (s *FileSink) Pageview(_ context.Context, rec Received) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.name("pv.json")
	s.Log.Info("captured pageview", "file", filepath.Base(p))
	return os.WriteFile(p, rec.Raw, 0o644)
}

// Analytics writes the analytics batch JSON body.
func (s *FileSink) Analytics(_ context.Context, rec Received) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.name("a.json")
	s.Log.Info("captured analytics", "file", filepath.Base(p), "events", len(rec.Analytics))
	return os.WriteFile(p, rec.Raw, 0o644)
}
