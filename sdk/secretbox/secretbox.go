// Package secretbox decrypts manifest-declared secret config fields
// at scan time (Stage I22 worker-side close).
//
// Workers consume controlplane-encrypted values that arrive as
// `enc:v1:<base64url nonce + ciphertext + tag>` strings in the
// merged config map. This package provides the matching Decrypt
// + DecryptFields helpers so worker-sdk's runtime.applyConfig can
// transparently substitute plaintext before invoking
// Configurable.ReloadConfig.
//
// The encrypt path lives in controlplane/internal/secretbox; we
// don't ship it here. Workers MUST NOT encrypt — that's the
// operator's UI flow only. Read-only on the worker side keeps
// the blast radius bounded: a compromised worker leaks secrets
// for the keys it holds, but can't poison the config column with
// attacker-controlled ciphertext.
//
// Format compatibility: this package reads the same `enc:v1:`
// envelope the controlplane writes. A future v2 needs both sides
// to support the new prefix; we'll add a Decrypt v2 path here in
// step with the controlplane bump.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Key is a 32-byte AES-256 key.
type Key [32]byte

const (
	prefixV1   = "enc:v1:"
	keyEnvName = "RECON_SECRETS_KEY"
	nonceLen   = 12
)

// ErrInvalid is returned for any tampering / wrong-key / corrupt
// ciphertext. We don't distinguish the cause — constant-time
// discipline + leaking less to a probe-attacker.
var ErrInvalid = errors.New("secretbox: ciphertext invalid or wrong key")

// ErrNoKey signals a missing/malformed RECON_SECRETS_KEY.
var ErrNoKey = errors.New("secretbox: RECON_SECRETS_KEY missing or malformed")

// LoadKeyFromEnv reads $RECON_SECRETS_KEY (base64 of 32 bytes).
// Returns ErrNoKey when missing / wrong-length / bad-base64.
//
// Workers call this once at boot. Missing key → workers can't
// decrypt; the SDK's applyConfig surfaces ErrInvalid on each
// secret field and ReloadConfig sees the literal "enc:v1:..."
// string. Operator visible: tool runs with garbage credentials
// until the env is fixed + worker restarts.
func LoadKeyFromEnv() (Key, error) {
	raw := os.Getenv(keyEnvName)
	if raw == "" {
		return Key{}, ErrNoKey
	}
	return ParseKey(raw)
}

// ParseKey decodes a base64-encoded 32-byte key string. Tolerates
// std + URL-safe encodings.
func ParseKey(b64 string) (Key, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		var err2 error
		raw, err2 = base64.RawURLEncoding.DecodeString(strings.TrimSpace(b64))
		if err2 != nil {
			return Key{}, fmt.Errorf("%w: %v", ErrNoKey, err)
		}
	}
	if len(raw) != 32 {
		return Key{}, fmt.Errorf("%w: key must be 32 bytes, got %d", ErrNoKey, len(raw))
	}
	var k Key
	copy(k[:], raw)
	return k, nil
}

// Decrypt reverses controlplane-side Encrypt. Returns ErrInvalid
// on every failure mode (wrong prefix, bad base64, wrong key,
// tag mismatch, truncated). Constant-time-ish: every error path
// hits the AEAD verify so a probe attacker can't time-leak
// "encrypted-with-my-key" vs "not-encrypted-at-all".
func Decrypt(k Key, encoded string) ([]byte, error) {
	if !strings.HasPrefix(encoded, prefixV1) {
		_ = constantTimeDummyVerify(k)
		return nil, ErrInvalid
	}
	body := encoded[len(prefixV1):]
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		_ = constantTimeDummyVerify(k)
		return nil, ErrInvalid
	}
	if len(raw) < nonceLen+16 {
		_ = constantTimeDummyVerify(k)
		return nil, ErrInvalid
	}
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, ct := raw[:nonceLen], raw[nonceLen:]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, ErrInvalid
	}
	return plaintext, nil
}

// IsEncrypted returns true when the value carries an "enc:vN:"
// sentinel. Persistence layers use this to decide whether to
// call Decrypt on read or pass through.
func IsEncrypted(s string) bool {
	return strings.HasPrefix(s, "enc:v") &&
		len(s) >= 7 &&
		s[6] == ':'
}

// constantTimeDummyVerify runs a fake AES-GCM seal so prefix-miss
// + bad-base64 paths don't return faster than tag-mismatch.
// Stdlib AES-GCM is constant-time on the data path; this guards
// the wrapper itself.
func constantTimeDummyVerify(k Key) error {
	block, err := aes.NewCipher(k[:])
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	dummy := make([]byte, nonceLen)
	_ = gcm.Seal(nil, dummy, []byte("x"), nil)
	return nil
}
