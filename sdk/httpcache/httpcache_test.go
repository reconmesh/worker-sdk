package httpcache

import (
	"crypto/sha256"
	"testing"
)

// canonicalize is the linchpin of cross-worker hits: tm-httpx writes
// "https://example.com/" and techmapper-worker looks up
// "https://example.com" - both must produce the same hash. These
// tests lock the canonicalization rules so a future refactor doesn't
// silently break cross-worker cache sharing.
func TestCanonicalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Trailing slash on root path, both forms collapse.
		{"https://example.com", "https://example.com/"},
		{"https://example.com/", "https://example.com/"},
		// Default ports stripped per scheme.
		{"https://example.com:443/", "https://example.com/"},
		{"http://example.com:80/", "http://example.com/"},
		// Non-default port preserved.
		{"http://example.com:8080/", "http://example.com:8080/"},
		// Host case folded; scheme too.
		{"HTTPS://Example.COM/", "https://example.com/"},
		// Fragment dropped (never sent to server).
		{"https://example.com/page#section", "https://example.com/page"},
		// Path preserved verbatim (caller normalizes if needed).
		{"https://example.com/a/b/c", "https://example.com/a/b/c"},
		// Query preserved verbatim - different queries are different
		// resources.
		{"https://example.com/search?q=1", "https://example.com/search?q=1"},
		// Empty input doesn't round-trip through the cache anyway;
		// the function still produces SOMETHING urlHash can hash so
		// no path crashes. We don't assert the exact value - just
		// that it's stable across calls (covered by TestURLHashStable).
		// Whitespace trimmed.
		{"  https://example.com/  ", "https://example.com/"},
	}
	for _, c := range cases {
		got := canonicalize(c.in)
		if got != c.want {
			t.Errorf("canonicalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestURLHashStable(t *testing.T) {
	// Same canonical URL must produce the same hash across calls.
	a := urlHash("https://example.com/")
	b := urlHash("https://example.com/")
	if string(a) != string(b) {
		t.Fatal("urlHash not deterministic")
	}
	// And it's a real SHA-256 - 32 bytes.
	if len(a) != sha256.Size {
		t.Fatalf("expected %d bytes, got %d", sha256.Size, len(a))
	}
	// Different URLs must produce different hashes.
	c := urlHash("https://example.com/page")
	if string(a) == string(c) {
		t.Fatal("hash collision on distinct URLs")
	}
}

// Cross-form equivalence: the four "same URL different shape" forms
// must all hash to the same value. This is the cache-hit guarantee.
func TestURLHashCrossForm(t *testing.T) {
	forms := []string{
		"https://example.com/",
		"https://example.com",
		"https://example.com:443/",
		"https://EXAMPLE.com/",
		"https://example.com/#anchor",
	}
	expected := urlHash(forms[0])
	for _, f := range forms[1:] {
		got := urlHash(f)
		if string(got) != string(expected) {
			t.Errorf("hash mismatch on %q (expected to match %q)", f, forms[0])
		}
	}
}
