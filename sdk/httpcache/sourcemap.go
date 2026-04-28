// sourcemap.go · persistence for original sources recovered from
// .map sourcemap v3 archives. Sister to the body cache (Upsert /
// Lookup) above, but writes a different table (tm_extracted_sources)
// keyed (url_hash, source_path) since one .map yields many files.
//
// Migration: 0016_extracted_sources.up.sql.
//
// Substrat published by TechMapper sourcemap module: bytes flow
// through Shared.Extra["sourcemap.extracted"] (mapURL → path → bytes).
// The asset URL the .map was referenced FROM is what we hash for
// url_hash · operators browsing /assets/{id}/sourcemap.zip want
// "every source recovered for this JS bundle", regardless of which
// vendored .map carried each file.
package httpcache

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SourceEntry is one (path, content) pair to upsert. Caller normalizes
// path before calling (cleanSourcePath in TechMapper) so the PK stays
// stable across re-fetches.
type SourceEntry struct {
	URL     string // asset URL the .map was referenced from
	Path    string // relative source path inside the .map
	Content []byte // raw source bytes
}

// SourceCache wraps the PG-backed source archive. Distinct from Cache
// (the body table) so consumers that don't write sources don't pull
// the type into their interface surface.
type SourceCache struct {
	pool *pgxpool.Pool
}

// NewSourceCache returns a SourceCache bound to pool. Pool ownership
// stays with the caller (matches the regular Cache constructor).
func NewSourceCache(pool *pgxpool.Pool) *SourceCache {
	return &SourceCache{pool: pool}
}

// Upsert writes a (url, path, content) tuple. Idempotent on the PK ·
// re-extracting the same .map after a body change overwrites the row,
// including fetched_at so the operator sees the freshest copy.
//
// Empty content / missing URL / missing path are caller bugs · we
// return an error rather than silently writing a sentinel.
func (c *SourceCache) Upsert(ctx context.Context, e SourceEntry) error {
	if e.URL == "" {
		return errors.New("httpcache: source entry URL required")
	}
	if e.Path == "" {
		return errors.New("httpcache: source entry Path required")
	}
	if len(e.Content) == 0 {
		return errors.New("httpcache: source entry Content required")
	}
	hash := urlHash(e.URL)
	sum := sha256.Sum256(e.Content)
	const q = `
INSERT INTO tm_extracted_sources (url_hash, source_path, content, sha256, fetched_at)
VALUES ($1, $2, $3, $4, now())
ON CONFLICT (url_hash, source_path) DO UPDATE SET
  content    = EXCLUDED.content,
  sha256     = EXCLUDED.sha256,
  fetched_at = now()
WHERE tm_extracted_sources.sha256 IS DISTINCT FROM EXCLUDED.sha256
   OR tm_extracted_sources.fetched_at < now() - INTERVAL '24 hours'
`
	if _, err := c.pool.Exec(ctx, q, hash, e.Path, e.Content, sum[:]); err != nil {
		return fmt.Errorf("httpcache: source upsert: %w", err)
	}
	return nil
}

// UpsertBatch is a convenience for the common case where a single
// .map yields many entries · we issue one INSERT per row but the
// caller saves a queue/round-trip per call site. Stops at first
// error to keep partial writes diagnosable; callers retrying should
// pass the residual slice.
func (c *SourceCache) UpsertBatch(ctx context.Context, entries []SourceEntry) error {
	for i := range entries {
		if err := c.Upsert(ctx, entries[i]); err != nil {
			return fmt.Errorf("httpcache: source upsert [%d]: %w", i, err)
		}
	}
	return nil
}

// SourceFile is a row from the source archive surface. Used by ZIP
// streaming code on the controlplane side.
type SourceFile struct {
	Path       string
	Content    []byte
	SHA256     []byte
	FetchedAtS string // ISO-ish, set by the loader if needed
}

// ListForURL returns every (path, content) pair recovered for the
// given asset URL. Hot path for the /assets/{id}/sourcemap.zip
// endpoint · the caller streams ZIP entries from this slice.
//
// Order is alphabetical by path so the resulting ZIP browses
// deterministically (operators diffing across runs see the same
// order). Limit guards against pathological cases where a .map
// dumps thousands of vendored sources past our extractor filter.
func (c *SourceCache) ListForURL(ctx context.Context, rawURL string, limit int) ([]SourceFile, error) {
	if rawURL == "" {
		return nil, errors.New("httpcache: source list URL required")
	}
	if limit <= 0 {
		limit = 5000
	}
	hash := urlHash(rawURL)
	const q = `
SELECT source_path, content, sha256
FROM tm_extracted_sources
WHERE url_hash = $1
ORDER BY source_path ASC
LIMIT $2
`
	rows, err := c.pool.Query(ctx, q, hash, limit)
	if err != nil {
		return nil, fmt.Errorf("httpcache: source list: %w", err)
	}
	defer rows.Close()
	out := make([]SourceFile, 0, 64)
	for rows.Next() {
		var sf SourceFile
		if err := rows.Scan(&sf.Path, &sf.Content, &sf.SHA256); err != nil {
			return nil, fmt.Errorf("httpcache: source list scan: %w", err)
		}
		// Defensive: trim trailing slashes / leading "./" in case an
		// older write predates the cleanSourcePath normalization. ZIP
		// archive entries can't start with / and a trailing / would
		// be interpreted as a directory by some unzip clients.
		sf.Path = strings.TrimPrefix(strings.TrimRight(sf.Path, "/"), "./")
		out = append(out, sf)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("httpcache: source list rows: %w", err)
	}
	return out, nil
}

// CountForURL returns the number of recovered sources for the URL ·
// cheap pre-flight for the UI badge ("12 files recoverable from .map")
// without dragging the bytes over the wire.
func (c *SourceCache) CountForURL(ctx context.Context, rawURL string) (int, error) {
	if rawURL == "" {
		return 0, errors.New("httpcache: count URL required")
	}
	hash := urlHash(rawURL)
	const q = `SELECT count(*)::int FROM tm_extracted_sources WHERE url_hash = $1`
	var n int
	if err := c.pool.QueryRow(ctx, q, hash).Scan(&n); err != nil {
		return 0, fmt.Errorf("httpcache: source count: %w", err)
	}
	return n, nil
}
