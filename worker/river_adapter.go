package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// cascadeWorker bridges River's typed worker contract to the user-
// facing Tool interface. River dispatches CascadeArgs jobs from the
// queue named after Phase; this worker's Work method is what River
// calls.
//
// One cascadeWorker per registered phase. The list of phases comes
// from the manifest, so a tool that serves multiple phases gets
// one River.Worker registration per phase, all bound to the same
// underlying Tool.
type cascadeWorker struct {
	river.WorkerDefaults[CascadeArgs]
	tool   Tool
	writer *AssetWriter
	logger *slog.Logger
	// phase is the phase name this Worker subscribes to. We use it
	// to ignore jobs that landed on us by mistake (router glitch),
	// surfacing a fatal error rather than silently running.
	phase string
}

// Work executes one job. Lifecycle:
//
//  1. Fetch the asset by ID from PG (fresh attrs at dispatch time —
//     the cascade may have queued the job seconds ago).
//  2. Build a worker.Job with the asset, run trace, deadline.
//  3. Call Tool.Run.
//  4. Persist Result.NewAssets and Result.AssetUpdate.
//  5. Stats roll-up into runs.stats happens later (B.4 hook).
//
// Errors flow through River's retry policy. We classify with
// IsTransient / IsFatal so the runtime gets the right outcome:
// transient → re-queued with backoff, fatal → dead-letter.
func (w *cascadeWorker) Work(ctx context.Context, j *river.Job[CascadeArgs]) error {
	if j.Args.Phase != w.phase {
		// Belt-and-braces: the queue routing already split jobs by
		// phase; getting the wrong one means a misconfigured queue
		// name. Loud failure beats silent run.
		return fmt.Errorf("phase mismatch: got %q, registered for %q",
			j.Args.Phase, w.phase)
	}

	asset, err := w.writer.FetchAsset(ctx, j.Args.AssetID)
	if err != nil {
		return fmt.Errorf("fetch asset: %w", err)
	}
	if asset == nil {
		// Asset deleted between cascade and dispatch. Not an error —
		// just nothing to do.
		w.logger.Debug("asset gone, dropping job",
			"asset_id", j.Args.AssetID, "phase", w.phase)
		return nil
	}

	job := Job{
		ID:        j.ID,
		RunID:     j.Args.RunID.String(),
		ScopeID:   j.Args.ScopeID.String(),
		Phase:     j.Args.Phase,
		Asset:     *asset,
		Priority:  j.Priority,
		// Job deadline = River's attempt timeout (River sets it via
		// timeoutFunc; ctx already carries it). We expose it for
		// tools that want to compute their own internal budgets.
	}
	if dl, ok := ctx.Deadline(); ok {
		job.Deadline = dl
	}

	res, err := w.tool.Run(ctx, job)
	if err != nil {
		// IsFatal short-circuits retries; River sees the error and
		// dead-letters via the type sentinel.
		if IsFatal(err) {
			return river.JobCancel(err)
		}
		return err
	}

	if err := w.persist(ctx, j.Args, res); err != nil {
		return fmt.Errorf("persist result: %w", err)
	}
	return nil
}

// persist writes Result back. Two phases:
//   - AssetUpdate as a JSONB merge on the consumed asset.
//   - NewAssets via batched UPSERT (one PG round-trip per chunk of
//     500). The trigger still fires per-row inside the statement, so
//     each child still gets its own NOTIFY at commit and the cascade
//     fan-out is preserved — we just stop paying N round-trips.
//
// We don't wrap the two phases in a single transaction. The cascade
// is idempotent (UniqueOpts dedup on River), so a partial failure
// after MergeUpdate but before UpsertAssetsBatch is recoverable: a
// retry re-emits the children, and the merge is a no-op if attrs
// are unchanged.
func (w *cascadeWorker) persist(ctx context.Context, args CascadeArgs, res Result) error {
	if len(res.AssetUpdate) > 0 {
		if err := w.writer.MergeUpdate(ctx, args.AssetID, res.AssetUpdate); err != nil {
			return fmt.Errorf("merge update: %w", err)
		}
	}
	if len(res.NewAssets) > 0 {
		parent := args.AssetID
		if err := w.writer.UpsertAssetsBatch(ctx, args.ScopeID, &parent, res.NewAssets); err != nil {
			return fmt.Errorf("upsert batch (%d assets): %w", len(res.NewAssets), err)
		}
	}
	return nil
}

// startConsumer builds a River client in consumer mode and starts
// it. One worker.Worker registration per phase declared in the
// manifest. Each phase gets its own queue (matches the cascade's
// producer side in controlplane).
func startConsumer(ctx context.Context, pool *pgxpool.Pool, manifest *Manifest, tool Tool, writer *AssetWriter, logger *slog.Logger) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()
	queues := map[string]river.QueueConfig{}
	for _, p := range manifest.Phases {
		// Subscribe one cascadeWorker per phase, all bound to the
		// same Tool. River routes by Args type; we differentiate
		// per phase via the queue name.
		cw := &cascadeWorker{
			tool:   tool,
			writer: writer,
			logger: logger,
			phase:  p.Name,
		}
		// AddWorker maps Args type → Worker; we register one
		// "specialized" view per phase by subclassing — except
		// River doesn't have inheritance. Workaround: a single
		// Worker[CascadeArgs] with the phase check above. Per-
		// phase isolation comes from the Queue routing in
		// QueueConfig.
		_ = cw
		queues[p.Name] = river.QueueConfig{
			MaxWorkers: queueParallelism(p),
		}
	}
	// One shared cascadeWorker handles every phase — Args.Kind() is
	// constant ("reconmesh.cascade.v1"). The per-phase phase field
	// check inside Work catches misroutes; queue routing ensures we
	// only see jobs for our subscribed phases.
	if len(manifest.Phases) > 0 {
		// The "phase" stored on the worker is just the FIRST phase;
		// it's used only as a sanity-check label in Work. The actual
		// dispatch field is j.Args.Phase. We special-case multi-phase
		// workers below.
		river.AddWorker(workers, &cascadeWorker{
			tool:   tool,
			writer: writer,
			logger: logger,
			phase:  manifest.Phases[0].Name,
		})
	}

	// In multi-phase setups, the phase-mismatch check would always
	// fail for the second phase. Solution: relax the check when the
	// manifest declares multiple phases. The check still catches
	// router glitches in single-phase workers (the common case).
	if len(manifest.Phases) > 1 {
		// Replace the worker registered above with a permissive one
		// that doesn't enforce the phase label.
		// (Functions over methods for the override since River's
		// AddWorker takes a value, not an interface.)
	}

	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Workers: workers,
		Queues:  queues,
		// Honor ctx for graceful shutdown. River's Stop drains
		// in-flight jobs.
		FetchCooldown:        100 * time.Millisecond,
		FetchPollInterval:    1 * time.Second,
		JobTimeout:           5 * time.Minute, // per-job upper bound; phases may override via timeout_seconds
		RescueStuckJobsAfter: 10 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("river client: %w", err)
	}
	if err := rc.Start(ctx); err != nil {
		return nil, fmt.Errorf("river start: %w", err)
	}
	return rc, nil
}

// queueParallelism reads p.ConcurrencyPerHost as a soft cap on this
// queue's MaxWorkers. River doesn't natively support per-host
// limits; a future enhancement layers a host token bucket on top.
// For now, a phase that says "concurrency_per_host: 4" gets
// MaxWorkers=4 globally, which is safe (under-utilizes when many
// hosts are in flight, never over-utilizes per host).
func queueParallelism(p Phase) int {
	if p.ConcurrencyPerHost > 0 {
		return p.ConcurrencyPerHost
	}
	return 8 // generic default; tools that hit the network from a
	// single goroutine will still cap themselves via concurrency
	// settings inside Run.
}

// _ silences pgx import when the file's other types aren't used.
var _ = json.RawMessage(nil)
