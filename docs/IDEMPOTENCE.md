# Idempotence

A worker's `Run(ctx, job)` MUST be idempotent. Re-invoking it with the
same `Job.ID` MUST produce the same `Result` (modulo external state
that legitimately changed in the meantime).

## Why it matters

The platform uses River, a Postgres-native job queue. River guarantees
**at-least-once delivery**: a job is redelivered when:

- the worker process crashes mid-run
- the worker doesn't ack within its timeout
- the operator manually replays a job
- a network partition cuts the worker from PG

If your `Run` produced side effects on the first attempt and the second
attempt repeats them, you double-emit findings, double-charge a
third-party API, double-write a file. The SDK protects you for the
parts under its control (asset upserts, finding dedup hashes), but
**external** side effects are your job.

## What the SDK guarantees for you

- **Asset upserts**: the runtime writes assets via
  `INSERT ... ON CONFLICT (scope_id, kind, value) DO UPDATE SET ...`
  Re-emitting the same asset from a retried `Run` only bumps
  `last_seen` + merges `attrs` - no duplicate row.

- **Finding dedup**: every finding's `(asset_id, dedup_hash)` is the
  uniqueness key. Identical findings re-emitted only update
  `last_seen`. The hash is computed from canonicalized
  `Kind + Severity + Data` - see `dedup.go`.

- **Job ack/nack semantics**: returning `(Result, nil)` ACKs and
  commits the result transactionally. Returning a non-nil error NACKs;
  the runtime decides retry vs dead-letter based on the error class
  (`ErrTransient` vs `ErrFatal`) and the job's retry budget.

## What you need to do

If your tool does **only**:

- Read from PG / archive
- Write findings + assets via the SDK return value
- Read from the network

…then you're already idempotent. The common case "fingerprint a URL
and emit one finding per detected technology" is safe.

You need to be careful when your tool:

### …calls third-party APIs that charge per request

A retry that re-calls Shodan / VirusTotal / paid DNS APIs costs you
money. Pattern: cache the response in PG (or `archive`), keyed by the
input. The first attempt reads from API + writes cache; the retry
reads from cache.

```go
func (t *MyTool) Run(ctx context.Context, j worker.Job) (worker.Result, error) {
    if cached, ok := loadCachedAPIResult(ctx, j.Asset.Value); ok {
        return resultFromCache(cached), nil
    }
    fresh, err := callExpensiveAPI(ctx, j.Asset.Value)
    if err != nil {
        return worker.Result{}, errors.Join(err, worker.ErrTransient)
    }
    saveCachedAPIResult(ctx, j.Asset.Value, fresh)
    return resultFromCache(fresh), nil
}
```

### …writes files to disk

Use the input asset's identity in the filename. Two retries write to
the same path; the second is a no-op (or safely overwrites). Avoid
`time.Now()` in filenames.

### …mutates PG outside the SDK return value

Don't, unless you know what you're doing. The SDK commits the
`Result` transactionally; arbitrary writes outside that transaction
can leave the DB in an inconsistent state on a crash between your
write and the SDK's. If you really need direct PG access, do it
inside the SDK's PG connection (exposed via `Job.Tx` in stage-two
runtime) so you share the transaction.

### …shells out to a subprocess

The subprocess might leave files, write to the network, etc. - apply
the rules above to whatever the subprocess does. Wrap it in a
deterministic key + cache layer.

## Testing for idempotence

The SDK ships a helper:

```go
// worker/test/idempotence.go
worker_test.AssertIdempotent(t, &MyTool{}, syntheticJob)
```

It runs `Run` twice in a row with the same job and asserts the
results are byte-equal. Add it to your worker's tests.

## Checklist before merging a worker

- [ ] No `time.Now()` in `Result.Findings[*].Data` or asset values.
- [ ] All third-party API calls are cached or otherwise replay-safe.
- [ ] No file writes outside `os.TempDir()`-style scratch space.
- [ ] No PG writes outside the SDK return value.
- [ ] `worker_test.AssertIdempotent` passes.
- [ ] If your tool mutates global state (a vendor list, a rule DB),
      it's behind `Updatable.Update`, called explicitly by the
      operator, never as a side effect of `Run`.
