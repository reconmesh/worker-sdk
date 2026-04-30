// SDK-side glue with the controlplane's api_keys central registry.
//
// Two paths greffées sur l'existing pgxpool:
//
//   1. fetchAPIKeyValue(id) decrypts the row's value_encrypted using
//      $RECON_SECRETS_KEY (already required for the existing
//      tool_configs.config secret-at-rest path). Called once per job
//      that arrives with a non-nil APIKeyID.
//
//   2. persistSecretFeedback(id, fb) writes ExpiresAt + upstream
//      quota columns. Called from the river adapter post-Run when
//      Result.SecretFeedback is set.
//
// Both functions are best-effort: any error logs and degrades to the
// legacy single-key path · we don't want a transient PG hiccup to
// fail-close on credential resolution.

package worker

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.vozec.fr/Parabellum/worker-sdk/sdk/secretbox"
)

// fetchAPIKeyValue resolves an api_keys row id to its decrypted
// plaintext. Returns the empty string + nil error when the row is
// missing or the secrets key isn't configured · caller falls back
// to legacy config.api_key in both cases.
func fetchAPIKeyValue(ctx context.Context, pool *pgxpool.Pool, key *secretbox.Key, id string) (string, error) {
	if pool == nil || key == nil || id == "" {
		return "", nil
	}
	var enc string
	err := pool.QueryRow(ctx,
		`SELECT value_encrypted FROM api_keys WHERE id = $1::uuid`, id,
	).Scan(&enc)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if !strings.HasPrefix(enc, "enc:v") {
		// Stored plaintext (shouldn't happen on writes through the
		// store; tolerate for ops migrations).
		return enc, nil
	}
	pt, err := secretbox.Decrypt(*key, enc)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// persistSecretFeedback flushes module-reported expiry / quota
// telemetry into api_keys. Each Set field overwrites; nil fields
// are left untouched · operators can mix module-reported expiry
// with manual quota_limit edits without one stomping the other.
func persistSecretFeedback(ctx context.Context, pool *pgxpool.Pool, id string, fb *SecretFeedback) error {
	if pool == nil || fb == nil || id == "" {
		return nil
	}
	sets := []string{"upstream_quota_seen_at = NOW()"}
	args := []any{id}
	if fb.ExpiresAt != nil {
		args = append(args, *fb.ExpiresAt)
		sets = append(sets, "expires_at = $2")
	}
	if fb.QuotaRemaining != nil {
		args = append(args, *fb.QuotaRemaining)
		sets = append(sets, "upstream_quota_remaining = $"+strconv.Itoa(len(args)))
	}
	if fb.QuotaLimit != nil {
		args = append(args, *fb.QuotaLimit)
		sets = append(sets, "upstream_quota_limit = $"+strconv.Itoa(len(args)))
	}
	_, err := pool.Exec(ctx,
		"UPDATE api_keys SET "+strings.Join(sets, ", ")+" WHERE id = $1::uuid",
		args...,
	)
	return err
}

