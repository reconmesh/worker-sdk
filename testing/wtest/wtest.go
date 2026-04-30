// Package wtest is a test harness for tm-* worker modules built on
// git.vozec.fr/Parabellum/worker-sdk/worker.
//
// It is purely additive: existing tests keep working, new tests opt in
// by importing this package. The goals are:
//
//   - Eliminate the boilerplate triplet that shows up in every module
//     test: build a [worker.Job], call the method, check shape of
//     [worker.Result] / [worker.HealthError].
//   - Standardize the way reload-config branches, healthcheck states
//     and error-class taxonomy get covered.
//   - Wrap [net/http/httptest] in a tiny mux + factory helpers so
//     "fake the upstream" is a one-liner.
//
// Usage sketch:
//
//	func TestRun_HappyPath(t *testing.T) {
//	    rt := wtest.NewRuntime(t)
//	    rt.HTTP.OnGET("/api", wtest.RespondJSON(200, map[string]any{"ok": true}))
//
//	    tool := &Tool{Endpoint: rt.HTTP.URL()}
//	    job := wtest.MockJob().WithKind("subdomain").WithValue("acme.com").Build()
//	    res, err := tool.Run(rt.Ctx, job)
//	    if err != nil {
//	        t.Fatalf("err: %v", err)
//	    }
//	    wtest.AssertEmits(t, res, "host", "1.2.3.4")
//	}
//
// The package depends only on the Go stdlib and on
// github.com/google/uuid (already pulled in by the SDK).
package wtest

import "git.vozec.fr/Parabellum/worker-sdk/worker"

// Severity aliases the worker package's severity scale so test files
// don't have to import the worker package solely for the constants.
type Severity = worker.Severity

const (
	SeverityInfo     = worker.SeverityInfo
	SeverityLow      = worker.SeverityLow
	SeverityMedium   = worker.SeverityMedium
	SeverityHigh     = worker.SeverityHigh
	SeverityCritical = worker.SeverityCritical
)
