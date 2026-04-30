package wtest

import (
	"errors"
	"fmt"
	"testing"

	"git.vozec.lan/Parabellum/worker-sdk/worker"
)

// recordT is a minimal testing.TB that records Errorf / Fatalf calls
// without failing the wrapping test. Lets us assert helpers correctly
// flag mismatches.
type recordT struct {
	testing.TB
	failed   bool
	messages []string
}

func newRecordT(t testing.TB) *recordT { return &recordT{TB: t} }

func (r *recordT) Errorf(format string, args ...any) {
	r.failed = true
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}
func (r *recordT) Fatalf(format string, args ...any) {
	r.failed = true
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}
func (r *recordT) Helper() {}

func TestAssertEmits_Pass(t *testing.T) {
	r := worker.Result{NewAssets: []worker.Asset{{Kind: "subdomain", Value: "a.acme.com"}}}
	rt := newRecordT(t)
	AssertEmits(rt, r, "subdomain", "a.acme.com")
	if rt.failed {
		t.Errorf("should pass: %v", rt.messages)
	}
}

func TestAssertEmits_FailsOnMissingValue(t *testing.T) {
	r := worker.Result{NewAssets: []worker.Asset{{Kind: "subdomain", Value: "a.acme.com"}}}
	rt := newRecordT(t)
	AssertEmits(rt, r, "subdomain", "b.acme.com")
	if !rt.failed {
		t.Errorf("should fail")
	}
}

func TestAssertEmits_FailsOnWrongKind(t *testing.T) {
	r := worker.Result{NewAssets: []worker.Asset{{Kind: "host", Value: "x"}}}
	rt := newRecordT(t)
	AssertEmits(rt, r, "subdomain", "x")
	if !rt.failed {
		t.Errorf("should fail")
	}
}

func TestAssertEmitsKind(t *testing.T) {
	r := worker.Result{NewAssets: []worker.Asset{{Kind: "ip", Value: "1.2.3.4"}}}
	rt := newRecordT(t)
	AssertEmitsKind(rt, r, "ip")
	if rt.failed {
		t.Errorf("should pass")
	}
	rt2 := newRecordT(t)
	AssertEmitsKind(rt2, r, "subdomain")
	if !rt2.failed {
		t.Errorf("should fail")
	}
}

func TestAssertEmitsCount(t *testing.T) {
	r := worker.Result{NewAssets: []worker.Asset{{Kind: "x"}, {Kind: "y"}}}
	rt := newRecordT(t)
	AssertEmitsCount(rt, r, 2)
	if rt.failed {
		t.Errorf("should pass")
	}
	rt2 := newRecordT(t)
	AssertEmitsCount(rt2, r, 5)
	if !rt2.failed {
		t.Errorf("should fail on count mismatch")
	}
}

func TestAssertNoEmits(t *testing.T) {
	rt := newRecordT(t)
	AssertNoEmits(rt, worker.Result{})
	if rt.failed {
		t.Errorf("should pass on empty")
	}
	rt2 := newRecordT(t)
	AssertNoEmits(rt2, worker.Result{NewAssets: []worker.Asset{{Kind: "x"}}})
	if !rt2.failed {
		t.Errorf("should fail on non-empty")
	}
}

func TestAssertFinding_PassAndFail(t *testing.T) {
	r := worker.Result{Findings: []worker.Finding{{Kind: "secret", Severity: SeverityHigh}}}
	rt := newRecordT(t)
	AssertFinding(rt, r, "secret", SeverityHigh)
	if rt.failed {
		t.Errorf("should pass: %v", rt.messages)
	}
	rt2 := newRecordT(t)
	AssertFinding(rt2, r, "secret", SeverityCritical)
	if !rt2.failed {
		t.Errorf("should fail on severity mismatch")
	}
}

func TestAssertFindingCount(t *testing.T) {
	r := worker.Result{Findings: []worker.Finding{{Kind: "a"}, {Kind: "b"}}}
	rt := newRecordT(t)
	AssertFindingCount(rt, r, 2)
	if rt.failed {
		t.Errorf("should pass")
	}
	rt2 := newRecordT(t)
	AssertFindingCount(rt2, r, 0)
	if !rt2.failed {
		t.Errorf("should fail on mismatch")
	}
}

func TestAssertNoFindings(t *testing.T) {
	rt := newRecordT(t)
	AssertNoFindings(rt, worker.Result{})
	if rt.failed {
		t.Errorf("should pass")
	}
	rt2 := newRecordT(t)
	AssertNoFindings(rt2, worker.Result{Findings: []worker.Finding{{Kind: "x"}}})
	if !rt2.failed {
		t.Errorf("should fail")
	}
}

func TestAssertAssetUpdate_DottedPath(t *testing.T) {
	r := worker.Result{
		AssetUpdate: map[string]any{
			"tm_foo": map[string]any{
				"matches": 3,
				"nested": map[string]any{
					"deep": "value",
				},
			},
		},
	}
	rt := newRecordT(t)
	AssertAssetUpdate(rt, r, "tm_foo.matches", 3)
	AssertAssetUpdate(rt, r, "tm_foo.nested.deep", "value")
	if rt.failed {
		t.Errorf("should pass: %v", rt.messages)
	}
}

func TestAssertAssetUpdate_WrongValue(t *testing.T) {
	r := worker.Result{AssetUpdate: map[string]any{"k": "v1"}}
	rt := newRecordT(t)
	AssertAssetUpdate(rt, r, "k", "v2")
	if !rt.failed {
		t.Errorf("should fail on wrong value")
	}
}

func TestAssertAssetUpdate_MissingPath(t *testing.T) {
	r := worker.Result{AssetUpdate: map[string]any{"k": "v"}}
	rt := newRecordT(t)
	AssertAssetUpdate(rt, r, "missing.path", "x")
	if !rt.failed {
		t.Errorf("should fail when path absent")
	}
}

func TestAssertAssetUpdatePresent(t *testing.T) {
	r := worker.Result{AssetUpdate: map[string]any{"x": "ok"}}
	rt := newRecordT(t)
	AssertAssetUpdatePresent(rt, r, "x")
	if rt.failed {
		t.Errorf("should pass")
	}
	rt2 := newRecordT(t)
	AssertAssetUpdatePresent(rt2, r, "missing")
	if !rt2.failed {
		t.Errorf("should fail on missing")
	}
}

func TestAssertErrorClass(t *testing.T) {
	err := worker.NewHealthError("rate_limited", "got 429")
	rt := newRecordT(t)
	AssertErrorClass(rt, err, "rate_limited")
	if rt.failed {
		t.Errorf("should pass: %v", rt.messages)
	}
	rt2 := newRecordT(t)
	AssertErrorClass(rt2, err, "service_unreachable")
	if !rt2.failed {
		t.Errorf("should fail on class mismatch")
	}
	rt3 := newRecordT(t)
	AssertErrorClass(rt3, errors.New("bare error"), "any")
	if !rt3.failed {
		t.Errorf("should fail when err isn't HealthError")
	}
	rt4 := newRecordT(t)
	AssertErrorClass(rt4, nil, "x")
	if !rt4.failed {
		t.Errorf("should fail on nil err")
	}
	// Empty class matches any HealthError class.
	rt5 := newRecordT(t)
	AssertErrorClass(rt5, err, "")
	if rt5.failed {
		t.Errorf("empty class should match any HealthError")
	}
}

func TestAssertHealth(t *testing.T) {
	hr := worker.HealthReport{Status: "healthy", Class: "ok"}
	rt := newRecordT(t)
	AssertHealth(rt, hr, "healthy", "")
	if rt.failed {
		t.Errorf("should pass: %v", rt.messages)
	}
	rt2 := newRecordT(t)
	AssertHealth(rt2, hr, "unhealthy", "dependency_missing")
	if !rt2.failed {
		t.Errorf("should fail")
	}
}

func TestAssertHealthExtra(t *testing.T) {
	hr := worker.HealthReport{Extra: map[string]any{"size": 7}}
	rt := newRecordT(t)
	AssertHealthExtra(rt, hr, "size", 7)
	if rt.failed {
		t.Errorf("should pass")
	}
	rt2 := newRecordT(t)
	AssertHealthExtra(rt2, hr, "missing", 1)
	if !rt2.failed {
		t.Errorf("should fail on missing key")
	}
}

func TestAssertStat(t *testing.T) {
	r := worker.Result{Stats: map[string]any{"requests": 10}}
	rt := newRecordT(t)
	AssertStat(rt, r, "requests", 10)
	if rt.failed {
		t.Errorf("should pass")
	}
	rt2 := newRecordT(t)
	AssertStat(rt2, r, "requests", 11)
	if !rt2.failed {
		t.Errorf("should fail on mismatch")
	}
}

func TestLookupPath_NilRoot(t *testing.T) {
	if _, ok := lookupPath(nil, "x"); ok {
		t.Errorf("lookup on nil root should return ok=false")
	}
}

func TestLookupPath_EmptyPath(t *testing.T) {
	if _, ok := lookupPath(map[string]any{"x": 1}, ""); ok {
		t.Errorf("empty path should return ok=false")
	}
}

func TestLookupPath_NonMapIntermediate(t *testing.T) {
	root := map[string]any{"a": "string-not-map"}
	if _, ok := lookupPath(root, "a.b"); ok {
		t.Errorf("non-map intermediate should return ok=false")
	}
}
