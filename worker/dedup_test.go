package worker

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestDedupHash_StableAcrossKeyOrder(t *testing.T) {
	a := Finding{Kind: "secret", Severity: SeverityHigh, Data: map[string]any{
		"file": "main.js", "match": "AKIA...", "line": 42,
	}}
	b := Finding{Kind: "secret", Severity: SeverityHigh, Data: map[string]any{
		"line": 42, "match": "AKIA...", "file": "main.js",
	}}
	if !bytes.Equal(DedupHash(a), DedupHash(b)) {
		t.Fatalf("hash differs by key order: %s vs %s",
			hex.EncodeToString(DedupHash(a)), hex.EncodeToString(DedupHash(b)))
	}
}

func TestDedupHash_TitleIgnored(t *testing.T) {
	a := Finding{Kind: "tech", Severity: SeverityInfo, Title: "Nginx",
		Data: map[string]any{"name": "Nginx", "version": "1.25"}}
	b := Finding{Kind: "tech", Severity: SeverityInfo, Title: "nginx (latest)",
		Data: map[string]any{"name": "Nginx", "version": "1.25"}}
	if !bytes.Equal(DedupHash(a), DedupHash(b)) {
		t.Fatal("title shouldn't affect dedup hash")
	}
}

func TestDedupHash_DataContentMatters(t *testing.T) {
	a := Finding{Kind: "tech", Severity: SeverityInfo,
		Data: map[string]any{"version": "1.25"}}
	b := Finding{Kind: "tech", Severity: SeverityInfo,
		Data: map[string]any{"version": "1.26"}}
	if bytes.Equal(DedupHash(a), DedupHash(b)) {
		t.Fatal("different data must hash differently")
	}
}

func TestDedupHash_SeverityMatters(t *testing.T) {
	a := Finding{Kind: "tech", Severity: SeverityInfo, Data: nil}
	b := Finding{Kind: "tech", Severity: SeverityHigh, Data: nil}
	if bytes.Equal(DedupHash(a), DedupHash(b)) {
		t.Fatal("different severity must hash differently")
	}
}

func TestDedupHash_NestedMapsCanonical(t *testing.T) {
	a := Finding{Kind: "x", Severity: SeverityLow, Data: map[string]any{
		"outer": map[string]any{"a": 1, "b": 2},
	}}
	b := Finding{Kind: "x", Severity: SeverityLow, Data: map[string]any{
		"outer": map[string]any{"b": 2, "a": 1},
	}}
	if !bytes.Equal(DedupHash(a), DedupHash(b)) {
		t.Fatal("nested map ordering should not affect hash")
	}
}

func TestDedupHash_SliceOrderMatters(t *testing.T) {
	// Slice order is intentionally significant - re-ordered evidence
	// is rare and ought to produce a distinct hash so re-runs that
	// happen to enumerate in a different order don't suppress what
	// might be a meaningful change.
	a := Finding{Kind: "x", Severity: SeverityLow, Data: map[string]any{
		"hits": []any{"a", "b"},
	}}
	b := Finding{Kind: "x", Severity: SeverityLow, Data: map[string]any{
		"hits": []any{"b", "a"},
	}}
	if bytes.Equal(DedupHash(a), DedupHash(b)) {
		t.Fatal("slice order should affect hash (preserved by design)")
	}
}

func TestDedupHash_EmptyDataStable(t *testing.T) {
	a := Finding{Kind: "x", Severity: SeverityInfo}
	b := Finding{Kind: "x", Severity: SeverityInfo}
	if !bytes.Equal(DedupHash(a), DedupHash(b)) {
		t.Fatal("empty findings should hash identically")
	}
}
