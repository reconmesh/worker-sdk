package worker

import (
	"bytes"
	"testing"
)

// TestFingerprintAttrsStable: same map produces the same hash
// regardless of insertion order. Pin equality across the
// canonicalize+pool refactor.
func TestFingerprintAttrsStable(t *testing.T) {
	a := map[string]any{"a": 1, "b": "x", "c": []any{1, 2}}
	b := map[string]any{"c": []any{1, 2}, "b": "x", "a": 1}
	ha := fingerprintAttrs(a)
	hb := fingerprintAttrs(b)
	if !bytes.Equal(ha, hb) {
		t.Errorf("same-content maps should hash equal: %x vs %x", ha, hb)
	}
}

// TestFingerprintAttrsDistinct: distinct content → distinct hash.
func TestFingerprintAttrsDistinct(t *testing.T) {
	a := map[string]any{"a": 1}
	b := map[string]any{"a": 2}
	if bytes.Equal(fingerprintAttrs(a), fingerprintAttrs(b)) {
		t.Error("distinct maps should hash distinct")
	}
}

// TestFingerprintAttrsEmpty: empty map is hashable, gives a fixed
// known prefix (sha256 of "[]\n" is stable).
func TestFingerprintAttrsEmpty(t *testing.T) {
	h := fingerprintAttrs(map[string]any{})
	if len(h) != 32 {
		t.Errorf("hash len = %d; want 32", len(h))
	}
	allZero := true
	for _, b := range h {
		if b != 0 {
			allZero = false
		}
	}
	if allZero {
		t.Error("empty hash should be non-zero (sha256 of canonical empty)")
	}
}

// TestFingerprintAttrsSliceOrderMatters: slices are NOT sorted ·
// reordering a slice changes the fingerprint by design (per the
// canonicalizeForHash comment).
func TestFingerprintAttrsSliceOrderMatters(t *testing.T) {
	a := map[string]any{"x": []any{1, 2, 3}}
	b := map[string]any{"x": []any{3, 2, 1}}
	if bytes.Equal(fingerprintAttrs(a), fingerprintAttrs(b)) {
		t.Error("slice reorder should change the fingerprint")
	}
}

// TestFingerprintAttrsConcurrentSafe: pool reuse must NOT leak state
// between callers. Run a few thousand fingerprints concurrently and
// assert deterministic equality persists.
func TestFingerprintAttrsConcurrentSafe(t *testing.T) {
	a := map[string]any{"k": "v", "n": 42, "arr": []any{1, 2, 3}}
	want := fingerprintAttrs(a)
	done := make(chan []byte, 64)
	for i := 0; i < 64; i++ {
		go func() {
			done <- fingerprintAttrs(map[string]any{"k": "v", "n": 42, "arr": []any{1, 2, 3}})
		}()
	}
	for i := 0; i < 64; i++ {
		got := <-done
		if !bytes.Equal(got, want) {
			t.Errorf("concurrent goroutine got %x; want %x", got, want)
			break
		}
	}
}

// BenchmarkFingerprintAttrs · ground-truth for the G1 sync.Pool
// optim claim. Run with `go test -run=^$ -bench=BenchmarkFingerprint
// -benchmem ./worker/...` to compare alloc/op + bytes/op against an
// unpooled reference. The plan called -27% bytes/op vs the plain
// json.Marshal+sha256.New each call.
func BenchmarkFingerprintAttrs(b *testing.B) {
	attrs := map[string]any{
		"ip":    "1.2.3.4",
		"port":  443,
		"tls":   true,
		"techs": []any{"nginx", "WordPress", "jQuery", "Bootstrap"},
		"http": map[string]any{
			"status_code": 200,
			"title":       "Example domain",
			"server":      "nginx/1.20.1",
		},
		"tls_meta": map[string]any{
			"cert_fingerprint": "deadbeef0123456789",
			"sans":             []any{"*.example.com", "example.com"},
			"version":          "TLS1.3",
			"cipher":           "TLS_AES_128_GCM_SHA256",
		},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = fingerprintAttrs(attrs)
	}
}
