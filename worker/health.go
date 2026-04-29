package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// envInt reads an int from env or returns fallback.
func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// healthLoop periodically calls the Tool's Healthcheck method (if it
// implements Healthchecker) and writes the result to module_health
// in PG. Runs every 60s (configurable via WORKER_HEALTH_INTERVAL_SECONDS).
//
// For tools that don't implement Healthchecker we still emit a
// passive heartbeat: the runtime tracks recent Run() outcomes and
// flips the row to "healthy" / "unhealthy" based on consecutive
// success/failure counts. That way every module gets a row in
// module_health regardless of whether it opts into the explicit
// interface.
func (rt *runtime) healthLoop(ctx context.Context) {
	intervalSec := envInt("WORKER_HEALTH_INTERVAL_SECONDS", 60)
	if intervalSec < 10 {
		intervalSec = 10
	}
	t := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer t.Stop()
	rt.reportHealth(ctx) // immediate so the row exists right after registration
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rt.reportHealth(ctx)
		}
	}
}

// reportHealth gathers the report and UPSERTs it. Order of
// preference for the report shape:
//   1. Tool.Healthcheck (explicit, opt-in)
//   2. Recent Run() outcomes (passive)
//   3. Default "unknown"
func (rt *runtime) reportHealth(ctx context.Context) {
	report := rt.gatherHealth(ctx)
	if err := rt.upsertHealth(ctx, report); err != nil {
		rt.logger.Warn("module_health upsert failed",
			"tool", rt.manifest.Tool, "error", err)
	}
}

func (rt *runtime) gatherHealth(ctx context.Context) HealthReport {
	if hc, ok := rt.tool.(Healthchecker); ok {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		report := hc.Healthcheck(probeCtx)
		if report.Status == "" {
			report.Status = "unknown"
		}
		if report.Class == "" {
			if report.Status == "healthy" {
				report.Class = "ok"
			} else {
				report.Class = "unknown"
			}
		}
		return report
	}
	// Passive · derive from Run() outcome counters.
	consecutiveFails := atomic.LoadInt32(&rt.consecutiveFailures)
	totalRuns := atomic.LoadInt32(&rt.totalRuns)
	if totalRuns == 0 {
		return HealthReport{Status: "unknown", Class: "unknown",
			Message: "no Run() invocations observed yet"}
	}
	switch {
	case consecutiveFails == 0:
		return HealthReport{Status: "healthy", Class: "ok"}
	case consecutiveFails >= 5:
		return HealthReport{
			Status:  "unhealthy",
			Class:   "unknown",
			Message: "5+ consecutive Run() errors · check worker logs",
		}
	default:
		return HealthReport{
			Status:  "degraded",
			Class:   "unknown",
			Message: "recent Run() errors · monitoring",
		}
	}
}

// upsertHealth writes one row to module_health using the Tool name
// as the PK. Fails-safe · a transient PG error doesn't kill the
// loop.
func (rt *runtime) upsertHealth(ctx context.Context, r HealthReport) error {
	if rt.pool == nil {
		return errors.New("no PG pool")
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	extraJSON, _ := json.Marshal(r.Extra)
	if extraJSON == nil || string(extraJSON) == "null" {
		extraJSON = []byte("{}")
	}
	const q = `
		INSERT INTO module_health
		    (tool, status, error_class, error_message, last_check_at,
		     last_success_at, last_failure_at,
		     consecutive_failures, failures_24h, successes_24h, extra)
		VALUES ($1, $2, $3, $4, now(),
		        CASE WHEN $2 = 'healthy' THEN now() ELSE NULL END,
		        CASE WHEN $2 != 'healthy' AND $2 != 'unknown' THEN now() ELSE NULL END,
		        CASE WHEN $2 != 'healthy' AND $2 != 'unknown' THEN 1 ELSE 0 END,
		        CASE WHEN $2 != 'healthy' AND $2 != 'unknown' THEN 1 ELSE 0 END,
		        CASE WHEN $2 = 'healthy' THEN 1 ELSE 0 END,
		        $5::jsonb)
		ON CONFLICT (tool) DO UPDATE SET
		    status = EXCLUDED.status,
		    error_class = EXCLUDED.error_class,
		    error_message = EXCLUDED.error_message,
		    last_check_at = now(),
		    last_success_at = CASE WHEN $2 = 'healthy' THEN now()
		                           ELSE module_health.last_success_at END,
		    last_failure_at = CASE WHEN $2 != 'healthy' AND $2 != 'unknown' THEN now()
		                           ELSE module_health.last_failure_at END,
		    consecutive_failures = CASE
		        WHEN $2 = 'healthy' THEN 0
		        WHEN $2 = 'unknown' THEN module_health.consecutive_failures
		        ELSE module_health.consecutive_failures + 1
		    END,
		    failures_24h = CASE
		        WHEN $2 != 'healthy' AND $2 != 'unknown'
		        THEN module_health.failures_24h + 1
		        ELSE module_health.failures_24h
		    END,
		    successes_24h = CASE
		        WHEN $2 = 'healthy'
		        THEN module_health.successes_24h + 1
		        ELSE module_health.successes_24h
		    END,
		    extra = $5::jsonb`
	_, err := rt.pool.Exec(wctx, q,
		rt.manifest.Tool, r.Status, r.Class, r.Message, string(extraJSON),
	)
	return err
}

// recordRunOutcome bumps the passive counters used by gatherHealth
// when the Tool doesn't implement Healthchecker. Called from the
// River adapter after each Run() invocation.
func (rt *runtime) recordRunOutcome(err error) {
	atomic.AddInt32(&rt.totalRuns, 1)
	if err == nil {
		atomic.StoreInt32(&rt.consecutiveFailures, 0)
		return
	}
	atomic.AddInt32(&rt.consecutiveFailures, 1)
	// If the worker returned a HealthError, fast-path it to the
	// upserter so the operator sees the failure class within
	// seconds rather than at the next 60s tick.
	var herr *HealthError
	if errors.As(err, &herr) {
		report := HealthReport{
			Status:  "unhealthy",
			Class:   herr.Class,
			Message: herr.Message,
		}
		go rt.upsertHealth(context.Background(), report)
	}
}
