// Command app is the single Fix Assistant binary.
//
//	app serve [--roles=ingest,web,worker] [--addr=127.0.0.1:8787]
//	app migrate up
//	app retention run
//	app admin create-project --org ORG --name NAME [--agent lovable] [--backend supabase] [--domains a.com,b.com] [--quota N]
//	app admin list-projects
//	app admin rotate-key --project ID
//	app admin set-quota --project ID --quota N
//	app admin login-link --email EMAIL
//	app spike-capture [--out=DIR] [--addr=127.0.0.1:8787]
//	app eval [--agent lovable] [--show NAME|all]
//
// Config comes from the environment: DATABASE_URL, INGEST_PUBLIC_URL,
// APP_PUBLIC_URL, APP_SECRET, STAFF_EMAILS, CONTACT_EMAIL, METRICS_TOKEN, GEOIP_DB, TRUST_PROXY,
// GITHUB_APP_ID + GITHUB_APP_SLUG + GITHUB_CLIENT_ID + GITHUB_CLIENT_SECRET + GITHUB_APP_PRIVATE_KEY,
// SMTP_HOST + SMTP_PORT + SMTP_USERNAME + SMTP_PASSWORD + EMAIL_FROM, or
// RESEND_API_KEY + EMAIL_FROM (else dev mailer, EMAIL_DEV_DIR).
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourname/fixassist/internal/agentapi"
	"github.com/yourname/fixassist/internal/ai"
	"github.com/yourname/fixassist/internal/alerts"
	"github.com/yourname/fixassist/internal/analytics"
	"github.com/yourname/fixassist/internal/auth"
	"github.com/yourname/fixassist/internal/github"
	"github.com/yourname/fixassist/internal/ingest"
	"github.com/yourname/fixassist/internal/jobs"
	"github.com/yourname/fixassist/internal/obs"
	"github.com/yourname/fixassist/internal/projects"
	"github.com/yourname/fixassist/internal/store"
	"github.com/yourname/fixassist/internal/store/db"
	"github.com/yourname/fixassist/internal/web"
)

const defaultAddr = "127.0.0.1:8787"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, log, os.Args[2:])
	case "migrate":
		err = migrate(ctx, log, os.Args[2:])
	case "retention":
		err = retention(ctx, log, os.Args[2:])
	case "admin":
		err = admin(ctx, log, os.Args[2:])
	case "spike-capture":
		err = spikeCapture(ctx, log, os.Args[2:])
	case "eval":
		err = evalCmd(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func logLevel() slog.Level {
	if os.Getenv("LOG_LEVEL") == "debug" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  app serve [--roles=ingest,web,worker] [--addr=127.0.0.1:8787]
  app migrate up
  app retention run
  app admin create-project --org ORG --name NAME [--agent lovable] [--backend supabase] [--domains a.com,b.com] [--quota N]
  app admin list-projects
  app admin rotate-key --project ID
  app admin set-quota --project ID --quota N
  app admin login-link --email EMAIL      (when EMAIL_MODE=off)
  app admin test-email --to EMAIL        (send one test email with the configured sender)
  app spike-capture [--out=DIR] [--addr=127.0.0.1:8787]
  app eval [--agent lovable] [--show NAME|all]   run the golden-set rubric
env: DATABASE_URL (required except spike-capture), INGEST_PUBLIC_URL (default http://<addr>)
`)
}

func openDB(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, errors.New("DATABASE_URL is not set")
	}
	return store.Open(ctx, url)
}

func ingestBase(addr string) string {
	if u := os.Getenv("INGEST_PUBLIC_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://" + addr
}

// staffEmails reads STAFF_EMAILS (comma-separated) for /admin.
func staffEmails() []string {
	var out []string
	for _, e := range strings.Split(os.Getenv("STAFF_EMAILS"), ",") {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// appSecret signs feedback links (APP_SECRET, at least 32 characters in
// production). Without it a random one is used and links die on restart.
func appSecret(log *slog.Logger) []byte {
	if s := os.Getenv("APP_SECRET"); len(s) >= 32 {
		return []byte(s)
	}
	log.Warn("APP_SECRET not set (or < 32 chars): using a random secret; email feedback links stop working after a restart")
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

// appBase is the dashboard URL used in sign-in links (APP_PUBLIC_URL).
func appBase(addr string) string {
	if u := os.Getenv("APP_PUBLIC_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://" + addr
}

func serve(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	roles := fs.String("roles", "ingest,web,worker", "comma-separated roles to run")
	addr := fs.String("addr", defaultAddr, "listen address")
	_ = fs.Parse(args)
	// Hosting platforms that assign the port (Northflank, Render, Koyeb, ...)
	// set PORT; it wins over the image's default --addr.
	if p := os.Getenv("PORT"); p != "" {
		*addr = ":" + p
	}

	enabled := map[string]bool{}
	for _, r := range strings.Split(*roles, ",") {
		r = strings.TrimSpace(r)
		switch r {
		case "ingest", "web", "worker":
			enabled[r] = true
		case "":
		default:
			return fmt.Errorf("unknown role %q", r)
		}
	}

	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, log); err != nil {
		return err
	}
	// Partitions must exist before the first insert; the daily job keeps them ahead.
	if err := jobs.RunPartitions(ctx, pool, log, time.Now()); err != nil {
		return err
	}

	sender := alerts.FromEnv(log)
	secret := appSecret(log)
	notifier := &alerts.Notifier{
		Pool: pool, Sender: sender, BaseURL: appBase(*addr), EmailOff: alerts.Off(),
		Prompt: ai.PromptForRow, Log: log,
		FeedbackLink: func(sid int64, outcome string) string {
			return web.FeedbackLink(appBase(*addr), secret, sid, outcome, time.Now())
		},
	}
	ghApp, err := github.FromEnv()
	if err != nil {
		return fmt.Errorf("github app: %w", err)
	}
	if ghApp == nil {
		// Saved by /admin/github's manifest flow (hosted deployments).
		if ghApp, err = github.LoadConfig(ctx, pool, secret); err != nil {
			log.Error("github app config not loaded; code context is off", "err", err)
			ghApp = nil
		}
	}
	var code ai.CodeSearcher
	if ghApp != nil {
		code = &github.Searcher{App: ghApp, Pool: pool}
		log.Info("github code context enabled", "app", ghApp.Slug)
	}
	riverClient, err := jobs.NewClientWith(jobs.Deps{Pool: pool, Log: log, Notifier: notifier, Code: code, RunWorkers: enabled["worker"]})
	if err != nil {
		return err
	}
	limits := projects.NewLimits()
	cache := &projects.Cache{Q: db.New(pool), Limits: limits, Log: log}
	if err := cache.Refresh(ctx); err != nil {
		return err
	}
	go cache.Run(ctx)

	enrich := &analytics.Enricher{Secret: secret, TrustProxy: os.Getenv("TRUST_PROXY") == "1"}
	if err := enrich.OpenGeo(os.Getenv("GEOIP_DB")); err != nil {
		log.Warn("GeoIP database not loaded; countries will be empty", "path", os.Getenv("GEOIP_DB"), "err", err)
	}
	metrics := obs.New(pool, log)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz(pool))
	mux.Handle("GET /metrics", metrics.Handler(os.Getenv("METRICS_TOKEN")))
	if enabled["ingest"] {
		(&ingest.Handler{
			Sink:     &ingest.PGSink{Pool: pool, River: riverClient},
			Log:      log,
			Projects: cache,
			Limits:   limits,
			Samples:  &ingest.SampleCap{},
			Wrap:     metrics.Wrap,
			Enrich:   enrich,
		}).Register(mux)
	}
	if enabled["web"] {
		(&web.SDK{Projects: cache, IngestBase: ingestBase(*addr), Log: log}).Register(mux)
		base := appBase(*addr)
		(&web.App{
			Pool:       pool,
			Auth:       &auth.Service{Pool: pool, Sender: sender, BaseURL: base},
			Projects:   &projects.Service{Pool: pool},
			IngestBase: ingestBase(*addr),
			BaseURL:    base,
			EmailOff:   alerts.Off(),
			Secret:     secret,
			Staff:      staffEmails(),
			Contact:    contactEmail(),
			Metrics:    metrics,
			GitHub:     ghApp,
			Code:       code,
			Log:        log,
		}).Register(mux)
		// The agent loop: MCP at /mcp, JSON at /agent/v1 (agent tokens).
		(&agentapi.Handler{Pool: pool, Code: code, Log: log}).Register(mux)
		log.Info("dashboard", "url", base)
	}
	if enabled["worker"] {
		if err := riverClient.Start(ctx); err != nil {
			return err
		}
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = riverClient.Stop(stopCtx)
		}()
	}
	log.Info("roles", "ingest", enabled["ingest"], "web", enabled["web"], "worker", enabled["worker"])
	return listen(ctx, log, *addr, mux)
}

func healthz(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if err := pool.Ping(ctx); err != nil {
				http.Error(w, "db down", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok"))
	}
}

func migrate(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) != 1 || args[0] != "up" {
		return errors.New("usage: app migrate up")
	}
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, log); err != nil {
		return err
	}
	return jobs.RunPartitions(ctx, pool, log, time.Now())
}

func retention(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) != 1 || args[0] != "run" {
		return errors.New("usage: app retention run")
	}
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	return jobs.RunPartitions(ctx, pool, log, time.Now())
}

func admin(ctx context.Context, log *slog.Logger, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: app admin create-project|list-projects|rotate-key|set-quota|login-link")
	}
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool, log); err != nil {
		return err
	}
	svc := &projects.Service{Pool: pool}
	base := ingestBase(defaultAddr)

	switch args[0] {
	case "create-project":
		fs := flag.NewFlagSet("create-project", flag.ExitOnError)
		org := fs.String("org", "", "organization name (created if new)")
		name := fs.String("name", "", "project name")
		agent := fs.String("agent", "other", "target AI agent: "+strings.Join(projects.Agents, ", "))
		backend := fs.String("backend", "unknown", "backend: "+strings.Join(projects.Backends, ", "))
		domains := fs.String("domains", "", "comma-separated allowed domains (empty = any)")
		quota := fs.Int64("quota", projects.DefaultQuotaMonthly, "monthly event quota")
		_ = fs.Parse(args[1:])
		var ds []string
		if *domains != "" {
			ds = strings.Split(*domains, ",")
		}
		p, err := svc.Create(ctx, projects.CreateParams{Org: *org, Name: *name, Agent: *agent, Backend: *backend, AllowedDomains: ds, QuotaMonthly: *quota})
		if err != nil {
			return err
		}
		dsn, err := projects.DSN(base, p.PublicKey, p.ID)
		if err != nil {
			return err
		}
		fmt.Printf("project_id: %d\npublic_key: %s\ndsn:        %s\nscript tag: <script data-fix-sdk src=\"%s/sdk/%s.js\"></script>\n",
			p.ID, p.PublicKey, dsn, base, p.PublicKey)
		return nil

	case "list-projects":
		rows, err := svc.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tORG\tNAME\tAGENT\tBACKEND\tQUOTA\tDOMAINS\tPUBLIC KEY")
		for _, r := range rows {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", r.ID, r.OrgName, r.Name, r.TargetAgent, r.Backend,
				r.QuotaMonthly, strings.Join(r.AllowedDomains, ","), r.PublicKey)
		}
		return tw.Flush()

	case "rotate-key":
		fs := flag.NewFlagSet("rotate-key", flag.ExitOnError)
		id := fs.Int64("project", 0, "project id")
		_ = fs.Parse(args[1:])
		key, err := svc.RotateKey(ctx, *id)
		if err != nil {
			return err
		}
		fmt.Printf("new public_key: %s (old keys stop working within %s)\nscript tag: <script data-fix-sdk src=\"%s/sdk/%s.js\"></script>\n",
			key, projects.RefreshEvery, base, key)
		return nil

	case "login-link":
		fs := flag.NewFlagSet("login-link", flag.ExitOnError)
		email := fs.String("email", "", "the person's email (their account is created on first sign-in)")
		_ = fs.Parse(args[1:])
		link, err := (&auth.Service{Pool: pool, BaseURL: appBase(defaultAddr)}).AdminLink(ctx, *email)
		if err != nil {
			return err
		}
		fmt.Printf("sign-in link for %s (single use, valid %s):\n%s\n", *email, auth.AdminLinkTTL, link)
		return nil

	case "test-email":
		// Sends one email with the configured sender, to check the settings.
		fs := flag.NewFlagSet("test-email", flag.ExitOnError)
		to := fs.String("to", "", "recipient")
		_ = fs.Parse(args[1:])
		if *to == "" {
			return errors.New("--to is required")
		}
		log := slog.New(slog.NewTextHandler(os.Stderr, nil))
		err := alerts.FromEnv(log).Send(ctx, alerts.Message{
			To: *to, Subject: "Fix Assistant: email is working",
			Text: "This is a test from Fix Assistant. Sign-in links, alerts and digests will be sent the same way.",
		})
		if err != nil {
			return err
		}
		fmt.Println("sent to", *to)
		return nil

	case "set-quota":
		fs := flag.NewFlagSet("set-quota", flag.ExitOnError)
		id := fs.Int64("project", 0, "project id")
		quota := fs.Int64("quota", 0, "monthly event quota")
		_ = fs.Parse(args[1:])
		if *quota <= 0 {
			return errors.New("--quota must be positive")
		}
		return svc.SetQuota(ctx, *id, *quota)
	}
	return fmt.Errorf("unknown admin command %q", args[0])
}

// evalCmd runs the golden set (FDR: "app eval --prompt=v2 ... reports changes").
func evalCmd(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	agent := fs.String("agent", "lovable", "agent style: "+strings.Join(projects.Agents, ", "))
	show := fs.String("show", "", "print the full prompt for one case name, or \"all\"")
	fromEnv := fs.String("from-envelopes", "", "instead of the golden set: explain captured envelopes (grouped by spike_case tag) and print the prompts")
	_ = fs.Parse(args)
	if *fromEnv != "" {
		return explainEnvelopes(*fromEnv, *agent)
	}
	results, err := ai.RunEval(*agent)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "prompt version %s, agent %s\n\nRESULT\tCASE\tORIGIN\tSEVERITY\tPROBLEMS\n", ai.PromptVersion, *agent)
	passed := 0
	for _, r := range results {
		status := "pass"
		if r.Pass() {
			passed++
		} else {
			status = "FAIL"
		}
		key := r.Suggestion.CatalogKey
		if key == "" {
			key = "agent"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", status, r.Case.Name, key, r.Suggestion.Severity, strings.Join(r.Failures, "; "))
	}
	_ = tw.Flush()
	fmt.Printf("\n%d/%d cases pass the rubric\n", passed, len(results))
	for _, r := range results {
		if *show == "all" || *show == r.Case.Name {
			fmt.Printf("\n=== %s\nTitle: %s\nWhat broke: %s\nWho: %s\nWhy: %s\n--- prompt for %s ---\n%s\n", r.Case.Name,
				r.Suggestion.Title, r.Suggestion.WhatBroke, r.Suggestion.WhoIsAffected, r.Suggestion.Why, *agent, r.Prompt)
		}
	}
	if passed < len(results) {
		os.Exit(1)
	}
	return nil
}

// explainEnvelopes runs captured envelopes through the production path
// (Prepare → context → Explain → agent style) and prints one prompt per
// spike case, for the Phase 4 fix check.
func explainEnvelopes(dir, agent string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.envelope"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	ctxs := map[string]*ai.Context{}
	users := map[string]map[string]bool{}
	var order []string
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		env, err := ingest.ParseEnvelope(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		for _, it := range env.Items {
			if it.Header.Type != ingest.ItemEvent {
				continue
			}
			row, err := ingest.Prepare([]byte("fixcheck"), it.Payload, time.Now())
			if err != nil {
				return err
			}
			ev, _ := ingest.ParseEvent(row.Payload)
			kase := ev.Tags["spike_case"]
			c, ok := ctxs[kase]
			if !ok {
				c = &ai.Context{Kind: row.Kind, Route: row.Route, Agent: agent, Backend: "supabase", Attempt: 1, FirstSeen: time.Now()}
				ctxs[kase], users[kase] = c, map[string]bool{}
				order = append(order, kase)
			}
			c.Events++
			users[kase][row.UserHash] = true
			c.Users = int64(len(users[kase]))
			c.AddSample(row.Payload)
		}
	}
	for _, kase := range order {
		s := ai.Explain(ctxs[kase])
		key := s.CatalogKey
		if key == "" {
			key = "agent (diagnose and fix)"
		}
		fmt.Printf("## %s\n\nOrigin: %s · severity %s\n\n**%s**\n\n%s\n\n```text\n%s\n```\n\n", kase, key, s.Severity, s.Title, s.WhatBroke, ai.ForAgent(agent, s))
	}
	return nil
}

// spikeCapture is the Phase 1 golden-file recorder: no database, any key.
func spikeCapture(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("spike-capture", flag.ExitOnError)
	out := fs.String("out", "internal/ingest/testdata/envelopes", "directory to write captured payloads")
	addr := fs.String("addr", defaultAddr, "listen address")
	_ = fs.Parse(args)
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz(nil))
	(&ingest.Handler{Sink: &ingest.FileSink{Dir: *out, Log: log}, Log: log}).Register(mux)
	(&web.SDK{IngestBase: ingestBase(*addr), Log: log}).Register(mux)
	log.Info("spike capture", "out", *out, "addr", *addr)
	return listen(ctx, log, *addr, mux)
}

// listen serves until ctx ends, then drains in-flight requests (10 s).
func listen(ctx context.Context, log *slog.Logger, addr string, h http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", addr)
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

// contactEmail is shown on the privacy page: CONTACT_EMAIL, else the first
// staff email.
func contactEmail() string {
	if e := strings.TrimSpace(os.Getenv("CONTACT_EMAIL")); e != "" {
		return e
	}
	if s := staffEmails(); len(s) > 0 {
		return s[0]
	}
	return ""
}
