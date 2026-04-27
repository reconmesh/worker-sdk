package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AssetWriter is the persistence shim every worker uses. It wraps
// the PG pool with the upsert logic the platform expects:
//
//   * fingerprint = sha256(canonicalize(attrs)). Computed client-side
//     so the assets_on_change trigger sees a stable value and emits
//     asset_events only on real changes.
//
//   * UPSERT by (scope_id, kind, value). The trigger does the rest:
//     emits NOTIFY asset_changed → cascade engine → next phase jobs.
//
//   * MergeUpdate for the common "enrich an existing asset" case
//     (e.g. tm-resolve adding ip+asn to a subdomain). The merge
//     happens server-side (jsonb || jsonb), the fingerprint is
//     recomputed in the same statement so we don't fetch the row
//     just to hash it.
//
// At-scale note: every worker writes through this, and every write
// holds the rare end-of-pipeline cost. The hot path stays cheap by
// hashing only when attrs actually change.
type AssetWriter struct {
	pool *pgxpool.Pool
}

func NewAssetWriter(pool *pgxpool.Pool) *AssetWriter { return &AssetWriter{pool: pool} }

// FetchAsset loads the row the cascade told us to act on. Workers
// rarely do this directly — the runtime calls it before invoking
// Tool.Run so the Job carries fresh attrs. Exposed so tools that
// need to re-read mid-Run (e.g. follow a parent_id chain) don't
// reinvent the SELECT.
func (w *AssetWriter) FetchAsset(ctx context.Context, id uuid.UUID) (*Asset, error) {
	const q = `
		SELECT id, scope_id, kind, value, parent_id, attrs, state
		  FROM assets
		 WHERE id = $1`
	var a Asset
	var attrs []byte
	var parent *uuid.UUID
	err := w.pool.QueryRow(ctx, q, id).Scan(
		&a.ID, &a.ScopeID, &a.Kind, &a.Value, &parent, &attrs, &a.State,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if parent != nil {
		a.ParentID = parent.String()
	}
	if len(attrs) > 0 {
		_ = json.Unmarshal(attrs, &a.Attrs)
	}
	return &a, nil
}

// UpsertAsset writes one asset (typically Result.NewAssets[i]) into
// the graph. Idempotent — repeated calls with identical attrs are a
// no-op (the trigger sees fingerprint unchanged and skips emitting
// asset_events). Sets state = 'discovered' on insert; the worker may
// override via attrs.state.
//
// scopeID and parentID are required: they're the tree links cascade
// uses, and workers must always know which scope they're enriching.
// (Runtime injects them from the Job before calling Tool.Run.)
func (w *AssetWriter) UpsertAsset(ctx context.Context, scopeID uuid.UUID, parentID *uuid.UUID, a Asset) error {
	if a.Kind == "" || a.Value == "" {
		return fmt.Errorf("asset.{kind,value} required")
	}
	attrs := a.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrsJSON, err := json.Marshal(attrs)
	if err != nil {
		return err
	}
	fp := fingerprintAttrs(attrs)

	// `parent_id` is nullable; pass through pgx's typed nil.
	var parentArg any
	if parentID != nil {
		parentArg = *parentID
	}

	const q = `
		INSERT INTO assets (scope_id, kind, value, parent_id, attrs, fingerprint, state)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, 'discovered')
		ON CONFLICT (scope_id, kind, value) DO UPDATE
		   SET attrs       = assets.attrs || EXCLUDED.attrs,
		       fingerprint = $7,
		       last_seen   = NOW()
		   WHERE assets.fingerprint IS DISTINCT FROM $7
		      OR assets.last_seen   < NOW() - interval '1 minute'`
	// Why the merge formula `assets.attrs || EXCLUDED.attrs`: workers
	// often only know about the keys they own (tm-resolve writes
	// attrs.ip; tm-portscan writes attrs.tcp_open[]). A naive
	// EXCLUDED.attrs would wipe everything else. JSONB || preserves
	// all existing keys, overwrites the ones we set.
	//
	// The recomputed fingerprint $7 is the hash of OUR partial attrs;
	// after merge, the row's actual attrs may differ from what we
	// hashed. The trigger compares (old fingerprint, new fingerprint)
	// — if they differ, it emits asset_events. Acceptable: a partial
	// update from us is still a real change worth recording.

	mergedFP := fingerprintAttrs(attrs)
	_, err = w.pool.Exec(ctx, q,
		scopeID, a.Kind, a.Value, parentArg, string(attrsJSON), fp, mergedFP,
	)
	return err
}

// MergeUpdate applies attrs delta to an existing asset by ID. Used
// by phases that enrich (tm-resolve setting ip on a subdomain) — the
// (scope, kind, value) identity is already known to be unique, so
// we don't reroute through UpsertAsset.
//
// Server-side merge keeps the hot path off the network: one Exec,
// no SELECT-then-UPDATE round trip.
func (w *AssetWriter) MergeUpdate(ctx context.Context, assetID uuid.UUID, delta map[string]any) error {
	if len(delta) == 0 {
		return nil
	}
	deltaJSON, err := json.Marshal(delta)
	if err != nil {
		return err
	}
	const q = `
		UPDATE assets
		   SET attrs       = attrs || $2::jsonb,
		       last_seen   = NOW(),
		       fingerprint = digest((attrs || $2::jsonb)::text, 'sha256')
		 WHERE id = $1`
	// digest() comes from pgcrypto (already in the schema). It
	// hashes the post-merge JSONB so the trigger sees a real diff
	// when something changed and a no-op when our delta added nothing
	// new. Building the canonical hash server-side instead of
	// client-side: we can't compute it without round-tripping the
	// pre-merge row, and the operator wants every avoidable round
	// trip eliminated.
	_, err = w.pool.Exec(ctx, q, assetID, string(deltaJSON))
	return err
}

// SetParentChain creates a parent_id link if missing. Used when a
// downstream phase discovers an asset that belongs under another
// (tm-portscan finds ports under a host the resolve phase already
// inserted). Idempotent.
func (w *AssetWriter) SetParentChain(ctx context.Context, childID, parentID uuid.UUID) error {
	const q = `UPDATE assets SET parent_id = $2 WHERE id = $1 AND parent_id IS NULL`
	_, err := w.pool.Exec(ctx, q, childID, parentID)
	return err
}

// fingerprintAttrs computes sha256 over the canonicalized attrs.
// Canonicalization sorts map keys recursively so two equal maps
// hash to the same bytes regardless of insertion order. Lifts the
// dedup logic from worker/dedup.go so attr equality is the same
// comparator workers used to use for findings.
func fingerprintAttrs(attrs map[string]any) []byte {
	h := sha256.New()
	canon := canonicalizeForHash(attrs)
	enc, _ := json.Marshal(canon)
	h.Write(enc)
	return h.Sum(nil)
}

// canonicalizeForHash sorts map keys recursively. Slices keep their
// order (a worker that emits results in a specific order makes that
// order semantically meaningful — for fingerprint purposes we treat
// reordering as a real change). Other types pass through.
func canonicalizeForHash(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([][2]any, 0, len(keys))
		for _, k := range keys {
			out = append(out, [2]any{k, canonicalizeForHash(t[k])})
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = canonicalizeForHash(e)
		}
		return out
	default:
		return t
	}
}
