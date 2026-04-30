package sourcecache

import (
	"errors"
	"testing"
)

// SourceCache.Upsert input validation. We don't run against a live
// pool here · the integration_test.go covers the actual PG round-trip.
// What's pinned: empty fields fail loudly rather than silently writing
// sentinel rows, since a sentinel would later confuse the ZIP stream
// and the secrets re-scan.
func TestSourceCacheUpsertValidation(t *testing.T) {
	cache := NewSourceCache(nil) // nil pool · validation runs before any query
	cases := []struct {
		name string
		e    SourceEntry
		want string
	}{
		{
			name: "empty URL",
			e:    SourceEntry{Path: "src/a.ts", Content: []byte("x")},
			want: "URL required",
		},
		{
			name: "empty Path",
			e:    SourceEntry{URL: "https://example.com/app.js", Content: []byte("x")},
			want: "Path required",
		},
		{
			name: "empty Content",
			e:    SourceEntry{URL: "https://example.com/app.js", Path: "src/a.ts"},
			want: "Content required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cache.Upsert(t.Context(), tc.e)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !errContains(err, tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// SourceCache.ListForURL / CountForURL: empty URL → error, no DB hit.
func TestSourceCacheListCountValidation(t *testing.T) {
	cache := NewSourceCache(nil)
	if _, err := cache.ListForURL(t.Context(), "", 10); err == nil {
		t.Fatal("ListForURL: expected error on empty URL")
	}
	if _, err := cache.CountForURL(t.Context(), ""); err == nil {
		t.Fatal("CountForURL: expected error on empty URL")
	}
}

// UpsertBatch: short-circuits on first error and reports the failing
// index so callers know how much of the slice landed. We put the
// invalid row first so the validation error fires before any DB hit
// (we don't have a live pool in this unit test).
func TestSourceCacheUpsertBatchShortCircuit(t *testing.T) {
	cache := NewSourceCache(nil)
	entries := []SourceEntry{
		{Path: "bad.ts"}, // missing URL → first failure, no DB hit
		{URL: "https://x/y.js", Path: "ok.ts", Content: []byte("x")},
	}
	err := cache.UpsertBatch(t.Context(), entries)
	if err == nil {
		t.Fatal("expected validation error from first entry, got nil")
	}
	if !errContains(err, "[0]") {
		t.Fatalf("expected error to name failing index [0], got %v", err)
	}
}

// Empty batch is a no-op · don't require a pool, don't error.
func TestSourceCacheUpsertBatchEmpty(t *testing.T) {
	cache := NewSourceCache(nil)
	if err := cache.UpsertBatch(t.Context(), nil); err != nil {
		t.Fatalf("empty batch should be a no-op, got %v", err)
	}
}

func errContains(err error, sub string) bool {
	if err == nil {
		return false
	}
	return contains(err.Error(), sub)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Sanity: NewSourceCache returns a non-nil pointer (caller would NPE
// on Upsert otherwise; nil-pool branch is reachable but only dies on
// the actual Exec).
func TestNewSourceCacheNotNil(t *testing.T) {
	if c := NewSourceCache(nil); c == nil {
		t.Fatal("NewSourceCache returned nil")
	}
}

// Compile-time: SourceEntry, SourceCache, SourceFile reachable from
// the package surface. Catches a refactor that accidentally moves
// them to internal.
var (
	_ = SourceEntry{}
	_ = (*SourceCache)(nil)
	_ = SourceFile{}
	_ = errors.New // import marker so the file compiles when the test body is gutted
)
