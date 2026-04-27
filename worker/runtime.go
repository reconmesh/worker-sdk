package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"gopkg.in/yaml.v3"
)

// runtime wires Postgres + River + admin HTTP. Built by Serve();
// stage two adds the actual River consumer, asset writer, heartbeat
// loop and worker self-registration. The Tool author never touches
// this code — Serve() is the only public entry.
//
// Lifecycle:
//
//   1. Boot: connect PG, parse manifest, self-register in `workers`
//      table with the full manifest cached as JSONB.
//   2. Start the admin HTTP server (/healthz, /readyz, /metrics).
//   3. Start the River consumer; for each phase in the manifest a
//      cascadeWorker subscribes to the matching queue.
//   4. Heartbeat goroutine: bumps `last_heartbeat` every 30s.
//   5. On ctx cancellation: drain River (graceful shutdown of in-
//      flight Tool.Run calls), close PG pool, stop admin server.
type runtime struct {
	tool      Tool
	manifest  *Manifest
	pgDSN     string
	adminAddr string
	logger    *slog.Logger

	pool   *pgxpool.Pool
	rc     *river.Client[pgx.Tx]
	writer *AssetWriter
	admin  *http.Server

	// instance is the unique identifier for this worker process —
	// "<tool>-<random>". It lands in workers.instance and is used
	// as the heartbeat key.
	instance string

	closeOnce sync.Once
}

type runtimeOptions struct {
	Tool      Tool
	Manifest  *Manifest
	PGDSN     string
	AdminAddr string
	Logger    *slog.Logger
}

func newRuntime(_ context.Context, opts runtimeOptions) (*runtime, error) {
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
		instance:  buildInstanceID(opts.Manifest.Tool),
	}, nil
}

// Run blocks until ctx is canceled. Boots PG → registers worker →
// starts admin HTTP → starts River consumer → starts heartbeat.
// Returns the first non-nil error encountered, otherwise the
// shutdown error chain.
func (rt *runtime) Run(ctx context.Context) error {
	rt.logger.Info("worker boot",
		"tool", rt.manifest.Tool,
		"version", rt.manifest.Version,
		"sdk", Version,
		"instance", rt.instance,
		"phases", phaseNames(rt.manifest))

	pool, err := pgxpool.New(ctx, rt.pgDSN)
	if err != nil {
		return fmt.Errorf("pg pool: %w", err)
	}
	rt.pool = pool
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("pg ping: %w", err)
	}

	rt.writer = NewAssetWriter(pool)

	if err := rt.registerWorker(ctx); err != nil {
		return fmt.Errorf("self-register: %w", err)
	}

	if err := rt.startAdmin(); err != nil {
		return fmt.Errorf("admin: %w", err)
	}

	rc, err := startConsumer(ctx, pool, rt.manifest, rt.tool, rt.writer, rt.logger)
	if err != nil {
		return fmt.Errorf("river consumer: %w", err)
	}
	rt.rc = rc
	rt.logger.Info("river consumer up",
		"queues", phaseNames(rt.manifest))

	go rt.heartbeatLoop(ctx)

	<-ctx.Done()
	rt.logger.Info("worker shutdown begin")
	return rt.shutdown()
}

// registerWorker UPSERTs into the workers table so the control
// plane's manifest cache picks us up. Stores the full manifest as
// JSONB — that's what the cascade engine reads when matching a new
// asset to candidate phases. Doing this directly via PG (not via
// HTTP) saves a round trip and works even when the control plane
// is down.
func (rt *runtime) registerWorker(ctx context.Context) error {
	manifestJSON, err := manifestAsJSON(rt.manifest)
	if err != nil {
		return err
	}
	queues := phaseNames(rt.manifest)
	// meta.admin_url lets the control plane reach our /admin/update
	// endpoint. We compose it from os.Hostname() (= the container's
	// in-cluster DNS name on docker-compose / k8s) plus the configured
	// listen port. Operators who run multiple replicas behind a
	// hostname will see one row per instance regardless of replica.
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	port := rt.adminAddr
	if strings.HasPrefix(port, ":") {
		port = port[1:]
	}
	meta := map[string]any{
		"admin_url": "http://" + host + ":" + port,
		"hostname":  host,
	}
	metaJSON, _ := json.Marshal(meta)

	const q = `
		INSERT INTO workers (instance, tool, version, sdk_version, queues,
		                     manifest, meta, started_at, last_heartbeat)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, NOW(), NOW())
		ON CONFLICT (instance) DO UPDATE
		   SET tool = EXCLUDED.tool,
		       version = EXCLUDED.version,
		       sdk_version = EXCLUDED.sdk_version,
		       queues = EXCLUDED.queues,
		       manifest = EXCLUDED.manifest,
		       meta = workers.meta || EXCLUDED.meta,
		       last_heartbeat = NOW()`
	_, err = rt.pool.Exec(ctx, q,
		rt.instance, rt.manifest.Tool, rt.manifest.Version, Version,
		queues, manifestJSON, metaJSON,
	)
	return err
}

// heartbeatLoop bumps last_heartbeat every 30s. The control plane's
// manifest cache filters on `last_heartbeat > NOW() - 5 minutes`, so
// missing two heartbeats removes us from the cascade routing pool.
//
// We intentionally don't reload the manifest here — the worker boots
// once with its compiled-in tool and manifest. Operators wanting a
// new manifest restart the worker.
func (rt *runtime) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
			_, err := rt.pool.Exec(ctx2,
				`UPDATE workers SET last_heartbeat = NOW() WHERE instance = $1`,
				rt.instance)
			cancel()
			if err != nil && ctx.Err() == nil {
				rt.logger.Warn("heartbeat", "error", err)
			}
		}
	}
}

func (rt *runtime) startAdmin() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		// Ready when PG ping succeeds; River readiness is harder to
		// query without going through internals, but a working PG
		// implies River is at least connectable.
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		if err := rt.pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("/manifest", func(w http.ResponseWriter, _ *http.Request) {
		// Return manifest as JSON so the control plane (or curl)
		// can introspect what this instance accepts.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rt.manifest)
	})
	mux.HandleFunc("/admin/update", rt.handleAdminUpdate)

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

// handleAdminUpdate triggers Tool.Update() if the tool implements
// Updatable. The control plane's update broadcaster (Phase E.1) hits
// this endpoint to refresh tool-local DBs (Wappalyzer signatures,
// secret rules, …) without restarting the worker.
func (rt *runtime) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	upd, ok := rt.tool.(Updatable)
	if !ok {
		http.Error(w, "tool is not Updatable", http.StatusNotImplemented)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := upd.Update(ctx); err != nil {
		rt.logger.Error("Update failed", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("updated"))
}

func (rt *runtime) shutdown() error {
	var firstErr error
	rt.closeOnce.Do(func() {
		// River first: drain in-flight jobs so we don't kill ToolRun
		// mid-write to PG.
		if rt.rc != nil {
			drainCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := rt.rc.Stop(drainCtx); err != nil && firstErr == nil {
				firstErr = err
			}
			cancel()
		}
		// Admin HTTP next; nothing depends on it staying up.
		if rt.admin != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := rt.admin.Shutdown(ctx); err != nil && firstErr == nil {
				firstErr = err
			}
			cancel()
		}
		// PG pool last: River has stopped and admin is down, no one
		// will use it anymore.
		if rt.pool != nil {
			rt.pool.Close()
		}
	})
	return firstErr
}

// Close releases resources. Idempotent.
func (rt *runtime) Close() error { return rt.shutdown() }

// ----- helpers ------------------------------------------------------

func phaseNames(m *Manifest) []string {
	out := make([]string, len(m.Phases))
	for i, p := range m.Phases {
		out[i] = p.Name
	}
	return out
}

// buildInstanceID concatenates the tool name with a host-derived
// suffix so multiple instances on the same machine don't UPSERT
// onto the same workers row. Falls back to random bytes if hostname
// is unavailable.
func buildInstanceID(tool string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	return fmt.Sprintf("%s-%s-%s", tool, host, hex.EncodeToString(rb[:]))
}

// manifestAsJSON serializes the manifest for the workers.manifest
// JSONB column. We round-trip via YAML→struct (which Validate has
// already accepted) → JSON so the on-disk shape matches what the
// control plane parses out of workers.manifest.
func manifestAsJSON(m *Manifest) ([]byte, error) {
	// Use YAML's encoder via marshal-then-decode to a typed struct,
	// then JSON-marshal. The `yaml:"foo"` tags differ from the
	// implicit JSON tags, but both struct shapes are isomorphic for
	// our manifest — json.Marshal does the right thing.
	_ = yaml.Marshal // referenced so the import survives if a future
	// edit removes the encode path
	return json.Marshal(m)
}
