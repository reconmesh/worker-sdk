package worker

import (
	"context"
	"errors"
	"testing"
)

// stubBulkRunner is a tiny BulkRunner implementation used to pin
// the interface contract from the SDK side · this makes it
// impossible to silently break BulkRunner via a future refactor
// of Tool.
type stubBulkRunner struct{}

func (stubBulkRunner) Name() string { return "stub-bulk" }

func (stubBulkRunner) Run(ctx context.Context, j Job) (Result, error) {
	// BulkRunner-bearing Tools still implement Run · the River
	// adapter falls back to it when BulkSize ≤ 1.
	return Result{}, errors.New("Run shouldn't be called when BulkSize > 1")
}

func (stubBulkRunner) RunBulk(ctx context.Context, jobs []Job) ([]Result, error) {
	out := make([]Result, len(jobs))
	for i := range jobs {
		out[i] = Result{Stats: map[string]any{"index": i}}
	}
	return out, nil
}

func (stubBulkRunner) BulkSize() int { return 50 }

// Compile-time pin: stubBulkRunner satisfies the BulkRunner contract.
var _ BulkRunner = stubBulkRunner{}

func TestBulkRunner_Returns1To1(t *testing.T) {
	tool := stubBulkRunner{}
	jobs := []Job{
		{Asset: Asset{Kind: "host", Value: "a.com"}},
		{Asset: Asset{Kind: "host", Value: "b.com"}},
		{Asset: Asset{Kind: "host", Value: "c.com"}},
	}
	results, err := tool.RunBulk(context.Background(), jobs)
	if err != nil {
		t.Fatalf("RunBulk: %v", err)
	}
	if len(results) != len(jobs) {
		t.Errorf("contract violation: got %d results for %d jobs", len(results), len(jobs))
	}
}

func TestBulkRunner_SizeContract(t *testing.T) {
	s := stubBulkRunner{}
	if s.BulkSize() <= 0 {
		t.Error("BulkSize ≤ 0 should mean fall-back · stub uses positive")
	}
}

func TestNewHealthError_PreservesClass(t *testing.T) {
	he := NewHealthError("api_key_invalid", "401 from upstream")
	if he.Class != "api_key_invalid" {
		t.Errorf("class lost: %q", he.Class)
	}
	if he.Error() != "api_key_invalid: 401 from upstream" {
		t.Errorf("Error() shape changed: %q", he.Error())
	}
}
