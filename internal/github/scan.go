package github

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/store/db"
)

var scanExt = map[string]bool{".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".vue": true, ".svelte": true, ".sql": true, ".mjs": true}

var skipDirs = []string{"node_modules/", "/dist/", "/build/", "/.next/", "/coverage/", "/vendor/", ".min.js"}

// maxFile skips huge (usually generated) files.
const maxFile = 512 << 10

// Scan searches a gzipped repo tarball for each term and returns up to 3
// references per term, preferring the term's preferred directory.
func Scan(tarball []byte, terms []ai.CodeTerm) ([]ai.CodeRef, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	found := make([][]ai.CodeRef, len(terms))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || h.Size > maxFile || !scanExt[path.Ext(h.Name)] {
			continue
		}
		// Tarballs are "<owner>-<repo>-<sha>/path"; drop the first segment.
		name := h.Name
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		skip := false
		for _, s := range skipDirs {
			if strings.Contains("/"+name, s) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64<<10), maxFile)
		line := 0
		for sc.Scan() {
			line++
			text := sc.Text()
			for i, t := range terms {
				if len(found[i]) < 20 && t.Pattern.MatchString(text) {
					found[i] = append(found[i], ai.CodeRef{Path: name, Line: line, Snippet: snippet(text), Term: t.Label})
				}
			}
		}
	}
	var out []ai.CodeRef
	for i, refs := range found {
		pref := terms[i].Prefer
		sort.SliceStable(refs, func(a, b int) bool {
			return pref != "" && strings.Contains(refs[a].Path, pref) && !strings.Contains(refs[b].Path, pref)
		})
		if len(refs) > 3 {
			refs = refs[:3]
		}
		out = append(out, refs...)
	}
	return out, nil
}

var reSpaces = regexp.MustCompile(`\s+`)

func snippet(s string) string {
	s = strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

// Searcher implements ai.CodeSearcher for projects with a connected repo.
// Tarballs are cached briefly so a burst of analyses downloads once.
type Searcher struct {
	App  *App
	Pool *pgxpool.Pool

	mu    sync.Mutex
	cache map[string]cachedTar
}

type cachedTar struct {
	data []byte
	at   time.Time
}

// Refs returns code references for a project, or nil if no repo is connected.
func (s *Searcher) Refs(ctx context.Context, projectID int64, terms []ai.CodeTerm) ([]ai.CodeRef, error) {
	if s == nil || s.App == nil || len(terms) == 0 {
		return nil, nil
	}
	g, err := db.New(s.Pool).GetProjectGitHub(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !g.RepoFullName.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	key := g.RepoFullName.String
	s.mu.Lock()
	c, ok := s.cache[key]
	s.mu.Unlock()
	if !ok || time.Since(c.at) > 10*time.Minute {
		data, err := s.App.Tarball(ctx, g.InstallationID, key)
		if err != nil {
			return nil, err
		}
		c = cachedTar{data: data, at: time.Now()}
		s.mu.Lock()
		if s.cache == nil {
			s.cache = map[string]cachedTar{}
		}
		s.cache[key] = c
		s.mu.Unlock()
	}
	return Scan(c.data, terms)
}
