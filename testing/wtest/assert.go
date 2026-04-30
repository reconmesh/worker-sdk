package wtest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"git.vozec.lan/Parabellum/worker-sdk/worker"
)

// AssertEmits fails the test if result.NewAssets does not contain at
// least one asset matching (kind, value). Both must match; the value
// match is exact.
func AssertEmits(t testing.TB, result worker.Result, kind, value string) {
	t.Helper()
	for _, a := range result.NewAssets {
		if a.Kind == kind && a.Value == value {
			return
		}
	}
	t.Errorf("AssertEmits: expected NewAsset kind=%q value=%q; got %s",
		kind, value, summarizeAssets(result.NewAssets))
}

// AssertEmitsKind passes if result.NewAssets has at least one asset of
// kind, regardless of value.
func AssertEmitsKind(t testing.TB, result worker.Result, kind string) {
	t.Helper()
	for _, a := range result.NewAssets {
		if a.Kind == kind {
			return
		}
	}
	t.Errorf("AssertEmitsKind: expected NewAsset kind=%q; got %s",
		kind, summarizeAssets(result.NewAssets))
}

// AssertEmitsCount asserts result.NewAssets has exactly n assets.
func AssertEmitsCount(t testing.TB, result worker.Result, n int) {
	t.Helper()
	if got := len(result.NewAssets); got != n {
		t.Errorf("AssertEmitsCount: got %d, want %d (assets: %s)",
			got, n, summarizeAssets(result.NewAssets))
	}
}

// AssertNoEmits fails if result has any NewAssets.
func AssertNoEmits(t testing.TB, result worker.Result) {
	t.Helper()
	if len(result.NewAssets) != 0 {
		t.Errorf("AssertNoEmits: expected zero, got %d (%s)",
			len(result.NewAssets), summarizeAssets(result.NewAssets))
	}
}

// AssertFinding asserts at least one Finding matches (kind, severity).
func AssertFinding(t testing.TB, result worker.Result, kind string, sev Severity) {
	t.Helper()
	for _, f := range result.Findings {
		if f.Kind == kind && f.Severity == sev {
			return
		}
	}
	t.Errorf("AssertFinding: expected kind=%q severity=%q; got %s",
		kind, sev, summarizeFindings(result.Findings))
}

// AssertFindingCount asserts result.Findings has exactly n elements.
func AssertFindingCount(t testing.TB, result worker.Result, n int) {
	t.Helper()
	if got := len(result.Findings); got != n {
		t.Errorf("AssertFindingCount: got %d, want %d (findings: %s)",
			got, n, summarizeFindings(result.Findings))
	}
}

// AssertNoFindings fails if result has any Findings.
func AssertNoFindings(t testing.TB, result worker.Result) {
	t.Helper()
	if len(result.Findings) != 0 {
		t.Errorf("AssertNoFindings: expected zero, got %d (%s)",
			len(result.Findings), summarizeFindings(result.Findings))
	}
}

// AssertAssetUpdate looks up dottedPath in result.AssetUpdate and
// compares the value with want using ==. Path elements are split on
// ".". Maps are descended; non-map intermediates fail.
//
// Example: AssertAssetUpdate(t, res, "tm_foo.matches", 3) reads
//
//	res.AssetUpdate["tm_foo"].(map[string]any)["matches"]
func AssertAssetUpdate(t testing.TB, result worker.Result, dottedPath string, want any) {
	t.Helper()
	got, ok := lookupPath(result.AssetUpdate, dottedPath)
	if !ok {
		t.Errorf("AssertAssetUpdate: path %q not present in AssetUpdate %v",
			dottedPath, result.AssetUpdate)
		return
	}
	if got != want {
		t.Errorf("AssertAssetUpdate: %s = %v (%T), want %v (%T)",
			dottedPath, got, got, want, want)
	}
}

// AssertAssetUpdatePresent asserts the dottedPath is present (any
// value) in AssetUpdate. Use when the exact value isn't important
// but the field's existence is.
func AssertAssetUpdatePresent(t testing.TB, result worker.Result, dottedPath string) {
	t.Helper()
	if _, ok := lookupPath(result.AssetUpdate, dottedPath); !ok {
		t.Errorf("AssertAssetUpdatePresent: path %q missing in AssetUpdate %v",
			dottedPath, result.AssetUpdate)
	}
}

// AssertErrorClass asserts err is a *worker.HealthError with the given
// class. Empty class matches any class.
func AssertErrorClass(t testing.TB, err error, class string) {
	t.Helper()
	if err == nil {
		t.Errorf("AssertErrorClass: expected HealthError class=%q, got nil error", class)
		return
	}
	var he *worker.HealthError
	if !errors.As(err, &he) {
		t.Errorf("AssertErrorClass: expected *worker.HealthError, got %T: %v", err, err)
		return
	}
	if class != "" && he.Class != class {
		t.Errorf("AssertErrorClass: class=%q, want %q (msg=%q)", he.Class, class, he.Message)
	}
}

// AssertHealth asserts the report's status + class. Empty class skips
// the class check (use when only the status matters, e.g. healthy
// states).
func AssertHealth(t testing.TB, hr worker.HealthReport, status, class string) {
	t.Helper()
	if hr.Status != status {
		t.Errorf("AssertHealth: status=%q, want %q (full=%+v)", hr.Status, status, hr)
	}
	if class != "" && hr.Class != class {
		t.Errorf("AssertHealth: class=%q, want %q (full=%+v)", hr.Class, class, hr)
	}
}

// AssertHealthExtra asserts the report's Extra map carries (key, want).
func AssertHealthExtra(t testing.TB, hr worker.HealthReport, key string, want any) {
	t.Helper()
	got, ok := hr.Extra[key]
	if !ok {
		t.Errorf("AssertHealthExtra: missing key %q in Extra=%v", key, hr.Extra)
		return
	}
	if got != want {
		t.Errorf("AssertHealthExtra: %s = %v, want %v", key, got, want)
	}
}

// AssertStat asserts result.Stats[key] == want.
func AssertStat(t testing.TB, result worker.Result, key string, want any) {
	t.Helper()
	got, ok := result.Stats[key]
	if !ok {
		t.Errorf("AssertStat: missing key %q in Stats=%v", key, result.Stats)
		return
	}
	if got != want {
		t.Errorf("AssertStat: %s = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

// lookupPath walks a dotted path through nested map[string]any. Returns
// the leaf value and true when present.
func lookupPath(root map[string]any, dottedPath string) (any, bool) {
	if root == nil || dottedPath == "" {
		return nil, false
	}
	parts := strings.Split(dottedPath, ".")
	var cur any = root
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func summarizeAssets(assets []worker.Asset) string {
	if len(assets) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(assets))
	for _, a := range assets {
		parts = append(parts, fmt.Sprintf("{%s=%s}", a.Kind, a.Value))
	}
	return strings.Join(parts, ", ")
}

func summarizeFindings(findings []worker.Finding) string {
	if len(findings) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, fmt.Sprintf("{kind=%s sev=%s title=%q}", f.Kind, f.Severity, f.Title))
	}
	return strings.Join(parts, ", ")
}
