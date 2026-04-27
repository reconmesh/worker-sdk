package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// runtime wires Postgres + River + admin HTTP. Built by Serve(); the
// implementation lives in this file so serve.go stays focused on
// parsing flags and orchestration.
//
// Stage one (this file): boots the admin server, logs subscription
// intent, blocks until ctx is canceled. Stage two (next PR) plugs in
// the actual River subscription, the PG worker registration row, the
// dedup-checked finding writes, and the cascade triggers.
//
// We split the work because River + pgx + the asset/finding writers
// each warrant their own dedicated file; landing them in one PR makes
// review intractable. The interface this struct exposes (Run / Close)
// won't change between stages.
type runtime struct {
	tool      Tool
	manifest  *Manifest
	pgDSN     string
	adminAddr string
	logger    *slog.Logger

	admin *http.Server
}

type runtimeOptions struct {
	Tool      Tool
	Manifest  *Manifest
	PGDSN     string
	AdminAddr string
	Logger    *slog.Logger
}

func newRuntime(ctx context.Context, opts runtimeOptions) (*runtime, error) {
	if opts.Tool == nil {
		return nil, errors.New("Tool required")
	}
	if opts.Manifest == nil {
		return nil, errors.New("Manifest required")
	}
	if opts.PGDSN == "" {
		return nil, errors.New("PG DSN required (PG_DSN env or -dsn flag)")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &runtime{
		tool:      opts.Tool,
		manifest:  opts.Manifest,
		pgDSN:     opts.PGDSN,
		adminAddr: opts.AdminAddr,
		logger:    opts.Logger,
	}, nil
}

// Run blocks until ctx is canceled. Stage-one body: just keep the
// admin endpoint alive so a tool author can boot the worker, hit
// /healthz, /metrics, and confirm the manifest validates.
func (rt *runtime) Run(ctx context.Context) error {
	rt.logger.Info("worker boot",
		"tool", rt.manifest.Tool,
		"version", rt.manifest.Version,
		"sdk", Version,
		"phases", phaseNames(rt.manifest))

	if err := rt.startAdmin(); err != nil {
		return fmt.Errorf("admin: %w", err)
	}

	// Placeholder: where the River subscription will land. For each
	// phase in the manifest we'll register a worker.Func against a
	// queue keyed by the phase name. Stage-two PR will populate this.
	for _, p := range rt.manifest.Phases {
		rt.logger.Info("phase ready (subscription pending stage-two)",
			"phase", p.Name,
			"consumes_kinds", p.Consumes.Kinds,
			"timeout_s", p.TimeoutSeconds,
		)
	}

	<-ctx.Done()
	rt.logger.Info("worker shutdown begin")
	return rt.shutdown()
}

func (rt *runtime) startAdmin() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		// Liveness only at this stage. Readiness will require a PG
		// ping + River subscription state once stage-two lands.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-yaml")
		// Best-effort echo: we don't re-marshal the in-memory struct
		// because comments would be lost. Until we add a /manifest
		// raw passthrough, return a tiny summary.
		fmt.Fprintf(w, "tool: %s\nversion: %s\nsdk_version: %s\n",
			rt.manifest.Tool, rt.manifest.Version, Version)
	})
	rt.admin = &http.Server{
		Addr:              rt.adminAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := rt.admin.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			rt.logger.Error("admin listen failed", "error", err)
		}
	}()
	rt.logger.Info("admin listening", "addr", rt.adminAddr)
	return nil
}

func (rt *runtime) shutdown() error {
	if rt.admin == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return rt.admin.Shutdown(ctx)
}

// Close releases resources. Today it's just the admin server (already
// drained in shutdown). Once PG + River join, this also drains the
// connection pool and unsubscribes River workers.
func (rt *runtime) Close() error { return nil }

func phaseNames(m *Manifest) []string {
	out := make([]string, len(m.Phases))
	for i, p := range m.Phases {
		out[i] = p.Name
	}
	return out
}
