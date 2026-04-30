# wtest - test harness for tm-* worker modules

`wtest` is a small package of helpers that factor out the boilerplate every
`tm-*` module's tests repeat: building `worker.Job` literals, mocking upstream
HTTP, asserting the shape of `worker.Result`, exercising `ReloadConfig`
branches and the `error_class` taxonomy on `worker.HealthError`.

The library is **additive**. Existing tests keep working; new tests opt in by
importing `git.vozec.fr/Parabellum/worker-sdk/testing/wtest`.

## What it covers

- **MockJob builder** - chainable, sane defaults, defensive copies on Build().
- **MockUpstream** - `httptest.Server` + tiny mux, factory respond helpers
  (`RespondJSON`, `RespondStatus`, `RespondString`, `RespondError`,
  `RespondHeader`, `RespondSequence`).
- **Runtime** - bundles MockUpstream + FakeDNS + FakeCache + a 5s context
  with a single `wtest.NewRuntime(t)` call. All cleanups wired via `t.Cleanup`.
- **Asserts** - `AssertEmits`, `AssertFinding`, `AssertAssetUpdate` (dotted
  path), `AssertErrorClass`, `AssertHealth`, plus their `*Count` / `No*` /
  `*Present` variants. All call `t.Helper()` so failures point at the test.
- **ReloadCases** - drive a `Configurable` through a table of reload
  scenarios; reflection reads the post-call field (exported or unexported).

No new dependencies. The package leans only on stdlib + `github.com/google/uuid`
(already pulled by the SDK).

## Before / after

A typical "set up tool, build job, call Run, inspect Result" test:

```go
// BEFORE
func TestRun_HappyPath(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(200)
        _, _ = w.Write([]byte(`{"hits":3}`))
    }))
    defer srv.Close()

    tool := &Tool{Endpoint: srv.URL, TimeoutSec: 5}
    res, err := tool.Run(context.Background(), worker.Job{
        ID:    1,
        RunID: "test-run",
        Asset: worker.Asset{
            ID:    "asset-1",
            Kind:  "host",
            Value: "acme.com",
            Attrs: map[string]any{},
        },
    })
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    enrich, ok := res.AssetUpdate["tm_foo"].(map[string]any)
    if !ok {
        t.Fatalf("no tm_foo update")
    }
    if enrich["hits"] != 3 {
        t.Errorf("hits = %v", enrich["hits"])
    }
}
```

```go
// AFTER
func TestRun_HappyPath(t *testing.T) {
    rt := wtest.NewRuntime(t)
    rt.HTTP.OnGET("/", wtest.RespondJSON(200, map[string]any{"hits": 3}))

    tool := &Tool{Endpoint: rt.HTTP.URL(), TimeoutSec: 5}
    job := wtest.MockJob().WithKind("host").WithValue("acme.com").Build()

    res, err := tool.Run(rt.Ctx, job)
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    wtest.AssertAssetUpdate(t, res, "tm_foo.hits", 3)
}
```

## Patterns it standardizes

- **Reload coverage**: `wtest.ReloadCases(t, tool, []wtest.ReloadCase{...})`
  drives the apply / wrong-type / zero-value triplet that every module's
  `ReloadConfig` needs.
- **Run kinds**: `wtest.MockJob().WithKind("url")...` — no more
  ten-line `worker.Job{Asset: worker.Asset{...}}` literals.
- **Healthcheck states**: `wtest.AssertHealth(t, hr, "unhealthy",
  "dependency_missing")` standardizes the assertion against the closed-set
  `error_class` taxonomy.
- **Error class**: `wtest.AssertErrorClass(t, err, "rate_limited")` covers
  the most common `*worker.HealthError` shape.

## The single import a downstream test file adds

```go
import "git.vozec.fr/Parabellum/worker-sdk/testing/wtest"
```

That's it.
