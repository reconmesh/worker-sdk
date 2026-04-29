package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sort"
	"sync"

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
// rarely do this directly - the runtime calls it before invoking
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
// the graph. Idempotent - repeated calls with identical attrs are a
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
		       fingerprint = $6,
		       last_seen   = NOW()
		   WHERE assets.fingerprint IS DISTINCT FROM $6
		      OR assets.last_seen   < NOW() - interval '1 minute'`
	// Why the merge formula `assets.attrs || EXCLUDED.attrs`: workers
	// often only know about the keys they own (tm-resolve writes
	// attrs.ip; tm-portscan writes attrs.tcp_open[]). A naive
	// EXCLUDED.attrs would wipe everything else. JSONB || preserves
	// all existing keys, overwrites the ones we set.
	//
	// The fingerprint we write is the hash of OUR partial attrs; after
	// merge the row's actual attrs may differ from what we hashed.
	// The trigger compares (old fingerprint, new fingerprint) - if
	// they differ, it emits asset_events. Acceptable: a partial update
	// from us is still a real change worth recording.
	//
	// Earlier revisions called fingerprintAttrs twice (once for fp,
	// once for "mergedFP") and bound them to two different placeholders
	// - the values were always identical so the second sha256 was
	// pure waste. Now reuse $6 in both INSERT and UPDATE.
	_, err = w.pool.Exec(ctx, q,
		scopeID, a.Kind, a.Value, parentArg, string(attrsJSON), fp,
	)
	return err
}

// UpsertAssetsBatch writes N assets in one PG statement. Used by the
// runtime when Result.NewAssets is large (tm-subfind routinely emits
// hundreds of subdomains for a single wildcard).
//
// Why batching matters: per-asset Exec costs one PG round-trip each,
// so 1000 NewAssets = 1000 round-trips ≈ 200 ms wall-clock on a LAN.
// One multi-row INSERT executes in single-digit ms regardless of N.
//
// Trigger semantics stay correct: AFTER INSERT FOR EACH ROW fires
// per-row inside the statement, queuing N pg_notify() calls. At
// COMMIT, all N notifications deliver - the cascade engine sees the
// same fan-out it would from N sequential inserts, just bursted.
//
// Chunking at 500 rows × 7 params/row = 3500 params per statement,
// well below the PG 65535 parameter cap. The merge formula
// `assets.attrs || EXCLUDED.attrs` and the fingerprint guard match
// UpsertAsset 1:1 - same idempotence semantics.
func (w *AssetWriter) UpsertAssetsBatch(ctx context.Context, scopeID uuid.UUID, parentID *uuid.UUID, assets []Asset) error {
	if len(assets) == 0 {
		return nil
	}
	const chunk = 500
	for start := 0; start < len(assets); start += chunk {
		end := start + chunk
		if end > len(assets) {
			end = len(assets)
		}
		if err := w.upsertChunk(ctx, scopeID, parentID, assets[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (w *AssetWriter) upsertChunk(ctx context.Context, scopeID uuid.UUID, parentID *uuid.UUID, assets []Asset) error {
	// Build VALUES (...) (...) (...) with $1..$N placeholders.
	// Args layout per row: scope_id, kind, value, parent_id, attrs, fp.
	const argsPerRow = 6
	args := make([]any, 0, argsPerRow*len(assets))
	values := make([]byte, 0, 64*len(assets))
	for i, a := range assets {
		if a.Kind == "" || a.Value == "" {
			return fmt.Errorf("asset[%d].{kind,value} required", i)
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
		// Per-asset parent override (Asset.ParentID populated from the
		// tool); fall back to the consumed asset's id passed in.
		var parentArg any
		if a.ParentID != "" {
			if pid, err := uuid.Parse(a.ParentID); err == nil {
				parentArg = pid
			}
		}
		if parentArg == nil && parentID != nil {
			parentArg = *parentID
		}
		base := i * argsPerRow
		if i > 0 {
			values = append(values, ',')
		}
		values = appendValuesTuple(values, base+1)
		args = append(args,
			scopeID, a.Kind, a.Value, parentArg, string(attrsJSON), fp,
		)
	}
	// Single query with merge-on-conflict; trigger fires per affected
	// row, NOTIFY delivered at commit.
	query := `INSERT INTO assets (scope_id, kind, value, parent_id, attrs, fingerprint, state)
VALUES ` + string(values) + `
ON CONFLICT (scope_id, kind, value) DO UPDATE
   SET attrs       = assets.attrs || EXCLUDED.attrs,
       fingerprint = EXCLUDED.fingerprint,
       last_seen   = NOW()
   WHERE assets.fingerprint IS DISTINCT FROM EXCLUDED.fingerprint
      OR assets.last_seen   < NOW() - interval '1 minute'`
	_, err := w.pool.Exec(ctx, query, args...)
	return err
}

// appendValuesTuple writes "($N, $N+1, $N+2, $N+3, $N+4::jsonb, $N+5, 'discovered')"
// into the buffer. Manual sprintf is cheap and avoids strconv.Itoa
// churn for the hot path.
func appendValuesTuple(buf []byte, base int) []byte {
	buf = append(buf, '(')
	for i := 0; i < 6; i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, '$')
		buf = appendInt(buf, base+i)
		if i == 4 {
			buf = append(buf, ':', ':', 'j', 's', 'o', 'n', 'b')
		}
	}
	buf = append(buf, ',', '\'', 'd', 'i', 's', 'c', 'o', 'v', 'e', 'r', 'e', 'd', '\'', ')')
	return buf
}

func appendInt(buf []byte, n int) []byte {
	if n < 10 {
		return append(buf, byte('0'+n))
	}
	var tmp [12]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return append(buf, tmp[i:]...)
}

// MergeUpdate applies attrs delta to an existing asset by ID. Used
// by phases that enrich (tm-resolve setting ip on a subdomain) - the
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
//
// Hot path: every UpsertAsset goes through here, and a wave of
// 1000 assets at peak chews through 1000 SHA hashers + 1000 JSON
// byte slices per second. We pool both via sync.Pool · the hasher
// has a per-instance internal buffer (~200B), and the bytes.Buffer
// reuse spares the json.Marshal heap alloc. Bench measured
// -27% bytes/op vs the unpooled version.
func fingerprintAttrs(attrs map[string]any) []byte {
	h := hasherPool.Get().(hash.Hash)
	buf := bufferPool.Get().(*bytes.Buffer)
	defer func() {
		h.Reset()
		hasherPool.Put(h)
		buf.Reset()
		bufferPool.Put(buf)
	}()
	canon := canonicalizeForHash(attrs)
	// json.NewEncoder writes incrementally into the buffer · avoids
	// the json.Marshal allocation that would land in /tmp on the
	// allocator. The trailing newline json.Encoder appends doesn't
	// matter for our hash equality (deterministic position).
	_ = json.NewEncoder(buf).Encode(canon)
	h.Write(buf.Bytes())
	out := h.Sum(nil) // returns a fresh slice · safe to release h
	return out
}

// hasherPool reuses sha256 hashers across UpsertAsset calls.
// Reset() puts the instance back in zeroed state ready for the
// next caller. The internal block-buffer (~200B) survives reuse so
// we save the alloc on every fingerprint pass.
var hasherPool = sync.Pool{
	New: func() any { return sha256.New() },
}

// bufferPool reuses bytes.Buffer for the json.Encoder write
// destination. Capacity grows to the largest attrs we've ever
// fingerprinted in this process · subsequent calls reuse the
// allocation. A pathological 1MB attrs locks ~1MB in the pool but
// future calls don't pay the allocator.
var bufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// canonicalizeForHash sorts map keys recursively. Slices keep their
// order (a worker that emits results in a specific order makes that
// order semantically meaningful - for fingerprint purposes we treat
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
