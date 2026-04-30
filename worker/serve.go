package worker

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"git.vozec.lan/Parabellum/worker-sdk/sdk/tracing"
)

// Serve is the worker's entry point. It:
//
//   1. Loads manifest.yaml (path overridable via -manifest).
//   2. Validates the manifest matches t.Name().
//   3. Connects to Postgres (PG_DSN env or -dsn flag).
//   4. Registers the worker in tm_workers (heartbeat goroutine).
//   5. Subscribes to River for every phase declared in the manifest.
//   6. Waits for SIGINT/SIGTERM, drains in-flight jobs, exits.
//
// Special modes (driven by flags):
//
//   --once --asset=<json>    run a single synthetic job, print Result, exit.
//                            No Postgres, no River. For local debugging.
//   --validate               load + validate manifest, exit 0/1. CI-friendly.
//
// Returning from Serve only happens on shutdown or fatal init error.
// Errors are surfaced via os.Exit(1) - workers are restartable processes,
// not libraries; we don't bubble errors up.
func Serve(t Tool) {
	cfg := parseFlags()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(cfg.LogLevel),
	}))
	slog.SetDefault(logger)

	manifest, err := LoadManifest(cfg.ManifestPath)
	if err != nil {
		die("manifest: %v", err)
	}
	if manifest.Tool != t.Name() {
		die("manifest tool=%q but Tool.Name()=%q - mismatch",
			manifest.Tool, t.Name())
	}
	// Best-effort attach README content. Default location is
	// ./README.md next to the binary; override via WORKER_README_PATH.
	// A missing file is non-fatal · the controlplane just shows a
	// "no docs" placeholder on the admin page.
	manifest.Readme = loadReadme(os.Getenv("WORKER_README_PATH"))
	if cfg.ValidateOnly {
		fmt.Printf("manifest %s v%s OK (%d phase(s))\n",
			manifest.Tool, manifest.Version, len(manifest.Phases))
		return
	}

	if cfg.OneShotAssetJSON != "" {
		runOnce(t, manifest, cfg)
		return
	}

	// Normal mode: connect to PG, register, subscribe to River. The
	// River wiring lives in runtime.go so this file stays focused on
	// orchestration.
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT, syscall.SIGTERM,
	)
	defer cancel()

	// OTel tracing - no-op when OTEL_EXPORTER_OTLP_ENDPOINT is unset.
	// Workers in dev or behind a private network skip the exporter
	// entirely; OTel-enabled deployments get spans around every Run.
	tracingShutdown, terr := tracing.Init(ctx, manifest.Tool, logger)
	if terr != nil {
		logger.Warn("tracing init", "error", terr)
	}
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = tracingShutdown(shutCtx)
	}()

	rt, err := newRuntime(ctx, runtimeOptions{
		Tool:       t,
		Manifest:   manifest,
		PGDSN:      cfg.PGDSN,
		AdminAddr:  cfg.AdminAddr,
		Logger:     logger,
	})
	if err != nil {
		die("runtime init: %v", err)
	}
	defer rt.Close()

	if err := rt.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		die("runtime: %v", err)
	}
}

// config holds resolved CLI/env values. Centralizing keeps Serve readable
// and makes the rare flag tweak a one-line change.
type config struct {
	ManifestPath     string
	PGDSN            string
	AdminAddr        string // metrics + healthz
	LogLevel         string
	ValidateOnly     bool
	OneShotAssetJSON string
}

func parseFlags() config {
	c := config{
		ManifestPath: defaultString("MANIFEST_PATH", manifestNextToBinary()),
		PGDSN:        os.Getenv("PG_DSN"),
		AdminAddr:    defaultString("ADMIN_ADDR", ":9090"),
		LogLevel:     defaultString("LOG_LEVEL", "info"),
	}
	flag.StringVar(&c.ManifestPath, "manifest", c.ManifestPath, "path to manifest.yaml")
	flag.StringVar(&c.PGDSN, "dsn", c.PGDSN, "PostgreSQL DSN (default $PG_DSN)")
	flag.StringVar(&c.AdminAddr, "admin", c.AdminAddr, "admin HTTP listen (metrics + healthz)")
	flag.StringVar(&c.LogLevel, "log-level", c.LogLevel, "debug|info|warn|error")
	flag.BoolVar(&c.ValidateOnly, "validate", false, "load + validate manifest, then exit")
	flag.StringVar(&c.OneShotAssetJSON, "asset", "",
		"JSON-encoded Asset to run once and exit (debug)")
	flag.Parse()
	return c
}

func defaultString(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	return fallback
}

func manifestNextToBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return "manifest.yaml"
	}
	return filepath.Join(filepath.Dir(exe), "manifest.yaml")
}

// loadReadme resolves the README path and returns its content as a
// string, or "" when missing or oversized. Truncation cap of 256 KiB
// keeps a runaway README out of the controlplane registration channel.
//
// Lookup order:
//   1. WORKER_README_PATH env if non-empty
//   2. ./README.md next to the binary
//   3. ./README.md relative to cwd (devloop case · `air` runs from src)
func loadReadme(override string) string {
	const maxBytes = 256 * 1024
	candidates := []string{}
	if override != "" {
		candidates = append(candidates, override)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "README.md"))
	}
	candidates = append(candidates, "README.md")
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if len(b) > maxBytes {
			b = b[:maxBytes]
		}
		return string(b)
	}
	return ""
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}

// runOnce executes Tool.Run against a synthetic job built from the
// supplied JSON Asset. No DB, no River - pure local debugging.
func runOnce(t Tool, m *Manifest, cfg config) {
	asset, err := parseOnceAsset(cfg.OneShotAssetJSON)
	if err != nil {
		die("asset: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := t.Run(ctx, Job{
		ID:       0,
		RunID:    "once",
		ScopeID:  asset.ScopeID,
		Phase:    m.Phases[0].Name,
		Asset:    asset,
		Priority: 4,
		Deadline: time.Now().Add(5 * time.Minute),
	})
	if err != nil {
		die("Run: %v", err)
	}
	printOnceResult(res)
}
