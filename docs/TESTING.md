# Testing tm-* modules

The recommended path for new test code in `tm-*` modules is the
[`worker-sdk/testing/wtest`](../testing/wtest/README.md) harness.

`wtest` factors out the boilerplate every module's tests share:

- Building `worker.Job` literals (random IDs, defensive attrs maps, sensible
  defaults for kind / scope / phase).
- Mocking upstream HTTP via `httptest.Server` + a tiny mux + factory helpers
  for the typical responses (`RespondJSON`, `RespondStatus`, `RespondString`,
  `RespondError`, `RespondHeader`, `RespondSequence`).
- Asserting the shape of `worker.Result` (emitted assets, findings,
  asset-update merges) without ad-hoc loops + `t.Errorf` triplets.
- Driving the `ReloadConfig` apply / wrong-type / zero-value triplet via a
  one-line table.
- Standardizing `error_class` taxonomy and `Healthcheck` state assertions.

## Quick start

```go
import "git.vozec.fr/Parabellum/worker-sdk/testing/wtest"

func TestRun_HappyPath(t *testing.T) {
    rt := wtest.NewRuntime(t) // HTTP + DNS + Cache + ctx, all auto-cleaned
    rt.HTTP.OnGET("/api", wtest.RespondJSON(200, map[string]any{"ok": true}))

    tool := &Tool{Endpoint: rt.HTTP.URL()}
    job := wtest.MockJob().WithKind("host").WithValue("acme.com").Build()

    res, err := tool.Run(rt.Ctx, job)
    if err != nil {
        t.Fatalf("err: %v", err)
    }
    wtest.AssertEmitsKind(t, res, "host")
    wtest.AssertNoFindings(t, res)
}
```

## Reload coverage

```go
wtest.ReloadCases(t, tool, []wtest.ReloadCase{
    {Name: "apply",      Cfg: map[string]any{"timeout_seconds": 30.0}, Field: "TimeoutSec", Want: 30},
    {Name: "wrong_type", Cfg: map[string]any{"timeout_seconds": "bogus"}, Field: "TimeoutSec", Want: 15},
    {Name: "zero_value", Cfg: map[string]any{"timeout_seconds": 0.0}, Field: "TimeoutSec", Want: 15},
})
```

Reflection reads the field after `ReloadConfig`, so unexported fields work
(no test-only `Get*` methods needed).

## Migration policy

The harness is **additive**. Existing tests are not rewritten; new tests adopt
`wtest`. A canonical parallel example sits in
[`modules/tm-wafprint/cmd/match_wtest_test.go`](../../modules/tm-wafprint/cmd/match_wtest_test.go).

For the full surface, see [`testing/wtest/README.md`](../testing/wtest/README.md).
