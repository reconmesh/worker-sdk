package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// makeKey + encryptForTest reimplement the controlplane-side
// Encrypt path inline so tests don't need to import the
// controlplane package (worker-sdk → controlplane import is the
// wrong direction). Same wire format: nonce || ciphertext || tag,
// gzipped... no, just AES-GCM. Match controlplane's wrapper
// exactly: prefixV1 + base64.RawURLEncoding(nonce ‖ sealed).
func makeKey(t *testing.T) Key {
	t.Helper()
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func encryptForTest(t *testing.T, k Key, plaintext []byte) string {
	t.Helper()
	block, err := aes.NewCipher(k[:])
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand: %v", err)
	}
	sealed := gcm.Seal(nil, nonce, plaintext, nil)
	out := append(nonce, sealed...)
	return prefixV1 + base64.RawURLEncoding.EncodeToString(out)
}

// Roundtrip pin: a value the controlplane would Encrypt must
// Decrypt back to the same plaintext on the worker side. A bug
// here breaks every secret-bearing worker.
func TestDecrypt_Roundtrip(t *testing.T) {
	k := makeKey(t)
	cases := [][]byte{
		[]byte("sk_live_abc123"),
		[]byte("hunter2"),
		[]byte(""), // empty plaintext is valid AES-GCM input
		[]byte(strings.Repeat("x", 1024)),
		// Binary
		{0x00, 0xff, 0xfe, 0x80, 0x00, 0x01},
	}
	for i, pt := range cases {
		ct := encryptForTest(t, k, pt)
		if !strings.HasPrefix(ct, "enc:v1:") {
			t.Errorf("[%d] missing prefix", i)
		}
		got, err := Decrypt(k, ct)
		if err != nil {
			t.Fatalf("[%d] Decrypt: %v", i, err)
		}
		if string(got) != string(pt) {
			t.Errorf("[%d] roundtrip mismatch", i)
		}
	}
}

// Wrong key fails with ErrInvalid, not panic.
func TestDecrypt_WrongKey(t *testing.T) {
	k1 := makeKey(t)
	k2 := makeKey(t)
	ct := encryptForTest(t, k1, []byte("secret"))
	got, err := Decrypt(k2, ct)
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("got %v, want ErrInvalid", err)
	}
	if got != nil {
		t.Errorf("got %q on wrong-key decrypt, want nil", got)
	}
}

// Tampered body → ErrInvalid (auth tag catches it).
func TestDecrypt_Tampered(t *testing.T) {
	k := makeKey(t)
	ct := []byte(encryptForTest(t, k, []byte("secret")))
	// Flip a byte in the base64 body.
	body := ct[len("enc:v1:"):]
	if body[0] == 'A' {
		body[0] = 'B'
	} else {
		body[0] = 'A'
	}
	_, err := Decrypt(k, string(ct))
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("tampered should ErrInvalid, got %v", err)
	}
}

// Missing prefix / non-base64 / truncated → ErrInvalid.
func TestDecrypt_BadInputs(t *testing.T) {
	k := makeKey(t)
	cases := []string{
		"plain text from before I22",
		"",
		"enc:v1:!!not-base64!!",
		"enc:v1:" + base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}), // too short
	}
	for _, in := range cases {
		_, err := Decrypt(k, in)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Decrypt(%q) = %v, want ErrInvalid", in, err)
		}
	}
}

// IsEncrypted: closed-set sentinel matcher.
func TestIsEncrypted(t *testing.T) {
	cases := map[string]bool{
		"":                                  false,
		"plain":                             false,
		"enc":                               false,
		"enc:v1":                            false,
		"enc:v1:":                           true,
		"enc:v1:abc":                        true,
		"enc:v2:future":                     true,
		"enc:vsomething:weird-but-prefixed": false,
	}
	for in, want := range cases {
		if got := IsEncrypted(in); got != want {
			t.Errorf("IsEncrypted(%q) = %v, want %v", in, got, want)
		}
	}
}

// LoadKeyFromEnv: missing → ErrNoKey; valid roundtrips.
func TestLoadKeyFromEnv(t *testing.T) {
	t.Setenv("RECON_SECRETS_KEY", "")
	if _, err := LoadKeyFromEnv(); !errors.Is(err, ErrNoKey) {
		t.Errorf("missing env should ErrNoKey, got %v", err)
	}
	raw := make([]byte, 32)
	raw[0] = 0xAB
	t.Setenv("RECON_SECRETS_KEY", base64.StdEncoding.EncodeToString(raw))
	k, err := LoadKeyFromEnv()
	if err != nil {
		t.Fatalf("valid env should load: %v", err)
	}
	if k[0] != 0xAB {
		t.Errorf("key[0] = %x", k[0])
	}
}

// ParseKey accepts std + URL-safe base64 (operators paste from
// many sources).
func TestParseKey_BothEncodings(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	std := base64.StdEncoding.EncodeToString(raw)
	url := base64.RawURLEncoding.EncodeToString(raw)

	k1, err := ParseKey(std)
	if err != nil {
		t.Errorf("std: %v", err)
	}
	k2, err := ParseKey(url)
	if err != nil {
		t.Errorf("url: %v", err)
	}
	if k1 != k2 {
		t.Error("std and url-safe of same bytes should match")
	}
	// Wrong-length → ErrNoKey.
	if _, err := ParseKey(base64.StdEncoding.EncodeToString(make([]byte, 16))); !errors.Is(err, ErrNoKey) {
		t.Errorf("short key should ErrNoKey, got %v", err)
	}
}

// Sentinel constants pinned. Renaming the prefix breaks every
// encrypted row already in production.
func TestSentinelConstants(t *testing.T) {
	if prefixV1 != "enc:v1:" {
		t.Errorf("prefixV1 drift: %q", prefixV1)
	}
	if keyEnvName != "RECON_SECRETS_KEY" {
		t.Errorf("keyEnvName drift: %q", keyEnvName)
	}
	if nonceLen != 12 {
		t.Errorf("nonceLen drift: %d", nonceLen)
	}
}

// DecryptFields: walks declared paths, decrypts encrypted
// strings, leaves plaintext + non-strings + empty alone, reports
// failures.
func TestDecryptFields_HappyPath(t *testing.T) {
	k := makeKey(t)
	ctAPI := encryptForTest(t, k, []byte("sk_live_abc"))
	ctOTX := encryptForTest(t, k, []byte("xyzzy"))

	cfg := map[string]any{
		"api_key":        ctAPI,
		"public_setting": "max_threads=10",
		"providers": map[string]any{
			"otx_token": ctOTX,
			"timeout":   30,
		},
	}
	dec, failed := DecryptFields(cfg, []string{"api_key", "providers.otx_token"}, k)
	if dec != 2 {
		t.Errorf("decrypted = %d, want 2", dec)
	}
	if len(failed) != 0 {
		t.Errorf("unexpected failures: %v", failed)
	}
	if cfg["api_key"] != "sk_live_abc" {
		t.Errorf("api_key not decrypted: %v", cfg["api_key"])
	}
	if cfg["public_setting"] != "max_threads=10" {
		t.Errorf("public field mutated")
	}
	if cfg["providers"].(map[string]any)["otx_token"] != "xyzzy" {
		t.Errorf("nested decrypt failed")
	}
	// Non-secret nested field untouched.
	if cfg["providers"].(map[string]any)["timeout"] != 30 {
		t.Errorf("non-secret mutated")
	}
}

// Failed decrypt reports the path AND leaves ciphertext in cfg
// (worker's HTTP call fails loudly with garbage credential —
// better than silently dropping the field and running unauth'd).
func TestDecryptFields_LeavesFailedAsCiphertext(t *testing.T) {
	k1 := makeKey(t)
	k2 := makeKey(t)
	ct := encryptForTest(t, k1, []byte("sk_live"))

	cfg := map[string]any{"api_key": ct}
	dec, failed := DecryptFields(cfg, []string{"api_key"}, k2) // wrong key
	if dec != 0 {
		t.Errorf("nothing should decrypt with wrong key, got %d", dec)
	}
	if len(failed) != 1 || failed[0] != "api_key" {
		t.Errorf("failed = %v, want [api_key]", failed)
	}
	// CRUCIAL: cfg["api_key"] is still the ciphertext. Worker's
	// downstream HTTP call will use it as a literal key and fail
	// auth — operator visible.
	if cfg["api_key"] != ct {
		t.Errorf("failed decrypt should leave ciphertext, got %v", cfg["api_key"])
	}
}

// Legacy plaintext (no enc:v1: prefix) passes through untouched
// — smooth migration path for pre-I22 columns.
func TestDecryptFields_LegacyPlaintextPassthrough(t *testing.T) {
	k := makeKey(t)
	cfg := map[string]any{"api_key": "legacy-plaintext"}
	dec, failed := DecryptFields(cfg, []string{"api_key"}, k)
	if dec != 0 || len(failed) != 0 {
		t.Errorf("legacy plaintext should pass through, dec=%d failed=%v",
			dec, failed)
	}
	if cfg["api_key"] != "legacy-plaintext" {
		t.Errorf("legacy mutated: %v", cfg["api_key"])
	}
}

// Empty / nil / non-string values skip cleanly.
func TestDecryptFields_SkipsNonStrings(t *testing.T) {
	k := makeKey(t)
	cfg := map[string]any{
		"api_key":     42,    // int, not a string
		"empty":       "",    // empty stays empty
		"missing":     nil,   // nil
	}
	dec, failed := DecryptFields(cfg, []string{"api_key", "empty", "missing", "absent"}, k)
	if dec != 0 || len(failed) != 0 {
		t.Errorf("non-string/empty/missing should skip, dec=%d failed=%v",
			dec, failed)
	}
}

// splitPath: dotted vs flat, empty-aware.
func TestSplitPath(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"api_key", []string{"api_key"}},
		{"providers.shodan_key", []string{"providers", "shodan_key"}},
		{"a.b.c", []string{"a", "b", "c"}},
		// Defensive against operator typos.
		{"providers.", []string{"providers"}},
		{".api_key", []string{"api_key"}},
		{"a..b", []string{"a", "b"}},
	}
	for _, c := range cases {
		got := splitPath(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitPath(%q): len %d, want %d", c.in, len(got), len(c.want))
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("splitPath(%q)[%d] = %q, want %q",
					c.in, i, got[i], c.want[i])
			}
		}
	}
}

// setValue is no-op when intermediate is missing / non-map (don't
// auto-create paths from typo'd secret declarations).
func TestSetValue_DoesNotAutoCreate(t *testing.T) {
	cfg := map[string]any{}
	setValue(cfg, []string{"a", "b"}, "x")
	if len(cfg) != 0 {
		t.Errorf("setValue created missing branch: %v", cfg)
	}
	// Setting an existing leaf works.
	cfg2 := map[string]any{"a": "old"}
	setValue(cfg2, []string{"a"}, "new")
	if cfg2["a"] != "new" {
		t.Errorf("leaf not updated: %v", cfg2)
	}
}
