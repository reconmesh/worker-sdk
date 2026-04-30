package worker

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"git.vozec.fr/Parabellum/worker-sdk/sdk/metrics"
	"git.vozec.fr/Parabellum/worker-sdk/sdk/secretbox"

	"gopkg.in/yaml.v3"
)

// runtime wires Postgres + River + admin HTTP. Built by Serve();
// stage two adds the actual River consumer, asset writer, heartbeat
// loop and worker self-registration. The Tool author never touches
// this code - Serve() is the only public entry.
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

	// instance is the unique identifier for this worker process -
	// "<tool>-<random>". It lands in workers.instance and is used
	// as the heartbeat key.
	instance string

	// secretsKey caches the result of secretbox.LoadKeyFromEnv()
	// across config reloads so we don't re-parse the env var on
	// every NOTIFY tool_config_changed. nil = not loaded yet (or
	// missing env). Loaded lazily on the first applyConfig call
	// that has manifest.Secrets non-empty.
	secretsKey *secretbox.Key

	// passive health counters · used when the Tool doesn't implement
	// the explicit Healthchecker interface. healthLoop derives a
	// status from these. River adapter bumps them after each Run().
	totalRuns           int32
	consecutiveFailures int32

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

	rc, err := startConsumer(ctx, rt, pool, rt.manifest, rt.tool, rt.writer, rt.logger)
	if err != nil {
		return fmt.Errorf("river consumer: %w", err)
	}
	rt.rc = rc
	rt.logger.Info("river consumer up",
		"queues", phaseNames(rt.manifest))

	go rt.heartbeatLoop(ctx)
	go rt.healthLoop(ctx)

	// Fire ReloadConfig once at boot with the merged manifest⊕override
	// config, then LISTEN for live edits. Skipped silently when the
	// Tool doesn't implement Configurable - the SDK doesn't force
	// every worker to opt in.
	if _, ok := rt.tool.(Configurable); ok {
		if err := rt.applyConfig(ctx); err != nil {
			rt.logger.Warn("initial config load",
				"tool", rt.manifest.Tool, "error", err)
		}
		go rt.configLoop(ctx)
	}

	<-ctx.Done()
	rt.logger.Info("worker shutdown begin")
	return rt.shutdown()
}

// applyConfig reads tool_configs.config for this tool, deep-merges
// it onto the manifest's static config, decrypts manifest-declared
// secret fields, and hands the result to
// Tool.ReloadConfig. Best-effort - a transient PG error logs and
// returns; the worker keeps running with whatever config it had.
//
// Merge rule (mirrors controlplane/internal/api/plugins.go):
// override wins on key conflict; nested maps recurse; arrays /
// scalars in override replace the manifest value wholesale.
//
// Decrypt rule: for every dotted-path in manifest.Secrets,
// if the merged value is "enc:v1:..." we Decrypt with
// $RECON_SECRETS_KEY. A failed decrypt leaves the ciphertext in
// place + logs the path - the worker's downstream HTTP call then
// fails loudly with garbage credentials, which is the right
// behavior (silently dropping the field would mean unauthenticated
// calls).
func (rt *runtime) applyConfig(ctx context.Context) error {
	cfg, ok := rt.tool.(Configurable)
	if !ok {
		return nil
	}
	merged := rt.fetchMergedConfig(ctx)
	rt.decryptSecrets(merged)
	rt.injectExternalLists(ctx, merged)
	loadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return cfg.ReloadConfig(loadCtx, merged)
}

// injectExternalLists pulls the latest content for every list this
// worker's manifest declares and writes it into the merged config
// under `lists.<name>.content` (raw bytes as a string). Workers
// read this in their own ReloadConfig handler.
//
// Source: external_lists table in the controlplane PG. We hit the
// content_gz column directly (single round-trip per list) so this
// adds at most N roundtrips per ReloadConfig pass · negligible vs
// the JSONB merge above.
func (rt *runtime) injectExternalLists(ctx context.Context, merged map[string]any) {
	if len(rt.manifest.ExternalLists) == 0 {
		return
	}
	lists := map[string]any{}
	for _, l := range rt.manifest.ExternalLists {
		content, hash, status, err := rt.fetchListContent(ctx, l.Name)
		if err != nil {
			rt.logger.Warn("external_list fetch",
				"name", l.Name, "error", err)
			continue
		}
		lists[l.Name] = map[string]any{
			"content":      string(content),
			"content_hash": hash,
			"status":       status,
		}
	}
	if len(lists) > 0 {
		merged["lists"] = lists
	}
}

// fetchListContent reads + gunzips one row's content_gz blob. Empty
// row (never fetched) returns an empty content string with status
// pending; that lets the worker fall back to its hard-coded default.
func (rt *runtime) fetchListContent(ctx context.Context, name string) ([]byte, string, string, error) {
	if rt.pool == nil {
		return nil, "", "", fmt.Errorf("no PG pool")
	}
	var (
		gz     []byte
		hash   string
		status string
	)
	err := rt.pool.QueryRow(ctx, `
		SELECT COALESCE(content_gz, ''::bytea),
		       COALESCE(content_hash, ''),
		       last_status
		  FROM external_lists
		 WHERE name = $1`, name).Scan(&gz, &hash, &status)
	if err != nil {
		return nil, "", "", err
	}
	if len(gz) == 0 {
		return nil, hash, status, nil
	}
	r, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, hash, status, err
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	return body, hash, status, err
}

// decryptSecrets is the I22 worker-side close. Read the cluster
// key once (cached on rt for subsequent reloads) and walk every
// declared secret path.
func (rt *runtime) decryptSecrets(merged map[string]any) {
	if len(rt.manifest.Secrets) == 0 {
		return
	}
	if rt.secretsKey == nil {
		k, err := secretbox.LoadKeyFromEnv()
		if err != nil {
			rt.logger.Warn("secretbox key not loaded - secret config fields will arrive as ciphertext",
				"tool", rt.manifest.Tool,
				"hint", "set RECON_SECRETS_KEY to a base64-encoded 32-byte value")
			return
		}
		rt.secretsKey = &k
	}
	dec, failed := secretbox.DecryptFields(merged, rt.manifest.Secrets, *rt.secretsKey)
	if dec > 0 {
		rt.logger.Info("decrypted secret config fields",
			"tool", rt.manifest.Tool, "count", dec)
	}
	if len(failed) > 0 {
		// Each failed field is left as ciphertext in merged so
		// the downstream call fails loudly. Operator visible:
		// the slog line tells them which field couldn't decrypt;
		// they rotate the key or re-paste the secret in the UI.
		rt.logger.Warn("secret decrypt failed - worker will see ciphertext for these fields",
			"tool", rt.manifest.Tool, "fields", failed)
	}
}

func (rt *runtime) fetchMergedConfig(ctx context.Context) map[string]any {
	out := map[string]any{}
	for k, v := range rt.manifest.Config {
		out[k] = v
	}
	var raw []byte
	err := rt.pool.QueryRow(ctx,
		`SELECT config FROM tool_configs WHERE tool = $1`,
		rt.manifest.Tool).Scan(&raw)
	if err != nil || len(raw) == 0 {
		return out
	}
	var override map[string]any
	if jerr := json.Unmarshal(raw, &override); jerr == nil {
		out = mergeConfigInto(out, override)
	}
	return out
}

// mergeConfigInto deep-merges b over a, returning a new map (a is
// not mutated). Mirrors the controlplane's behavior so what the UI
// shows as "effective" is exactly what the worker sees.
func mergeConfigInto(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		if vmap, vok := v.(map[string]any); vok {
			if existing, eok := out[k].(map[string]any); eok {
				out[k] = mergeConfigInto(existing, vmap)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// configLoop subscribes to PG NOTIFY 'tool_config_changed' and fires
// ReloadConfig whenever the operator edits this tool's override.
// One LISTEN connection per worker - cheap; pgx pool reserves it.
func (rt *runtime) configLoop(ctx context.Context) {
	conn, err := rt.pool.Acquire(ctx)
	if err != nil {
		rt.logger.Warn("config listen: acquire", "error", err)
		return
	}
	// Nil-guarded defer: when the reconnect path below releases conn
	// and the subsequent Acquire fails (PG fully down), conn is set
	// to nil and we return. Without the guard, the defer would call
	// nil.Release() and panic the entire worker process on a PG flap.
	defer func() {
		if conn != nil {
			conn.Release()
		}
	}()
	if _, err := conn.Exec(ctx, `LISTEN tool_config_changed`); err != nil {
		rt.logger.Warn("config listen: LISTEN", "error", err)
		return
	}
	for {
		notif, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			rt.logger.Warn("config listen: notify", "error", err)
			// Reconnect attempt: brief backoff, then re-acquire +
			// re-LISTEN. Keeps the worker reactive across PG flaps.
			time.Sleep(2 * time.Second)
			conn.Release()
			conn = nil // defer is safe even if Acquire below fails
			conn, err = rt.pool.Acquire(ctx)
			if err != nil {
				return
			}
			_, _ = conn.Exec(ctx, `LISTEN tool_config_changed`)
			continue
		}
		// Payload is the bare tool name. Skip notifications for other
		// tools so a 100-tool cluster doesn't trigger 100 reloads on
		// every edit.
		if notif.Payload != rt.manifest.Tool {
			continue
		}
		if err := rt.applyConfig(ctx); err != nil {
			rt.logger.Warn("config reload",
				"tool", rt.manifest.Tool, "error", err)
		} else {
			rt.logger.Info("config reloaded", "tool", rt.manifest.Tool)
		}
	}
}

// registerWorker UPSERTs into the workers table so the control
// plane's manifest cache picks us up. Stores the full manifest as
// JSONB - that's what the cascade engine reads when matching a new
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
// We intentionally don't reload the manifest here - the worker boots
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
	// Prometheus metrics - labels carry tool + phase + outcome.
	// Mounted on the same admin port so a single scrape config
	// covers every replica without per-worker port-mapping.
	mux.Handle("/metrics", metrics.Handler())

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
	// our manifest - json.Marshal does the right thing.
	_ = yaml.Marshal // referenced so the import survives if a future
	// edit removes the encode path
	return json.Marshal(m)
}
