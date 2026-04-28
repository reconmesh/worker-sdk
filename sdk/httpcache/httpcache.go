// Package httpcache is the cluster-wide HTTP body cache. tm-httpx
// writes after each fingerprint scan; techmapper-worker (and future
// secrets / sourcemap / link-extractor plugins) read here before
// issuing a network fetch — between recheck waves nothing leaves the
// cluster, only the 24h cron sweep refreshes.
//
// Backed by PG table `tm_http_bodies` (see
// recon-platform/migrations/0001_init.up.sql). Lookup is keyed by a
// SHA-256 of the canonical URL so PK lookups are fixed-cost
// regardless of URL length.
//
// Workers open one Cache per process. The struct is goroutine-safe
// (it wraps a pgxpool); callers can stash it on their Tool struct.
package httpcache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// Hop is one entry in a redirect chain.
type Hop struct {
	Status   int    `json:"status"`
	Location string `json:"location,omitempty"`
}

// Entry is one row in tm_http_bodies. The Cache hands these to
// callers; readers consume Body / Headers / StatusCode and can
// avoid issuing a fresh network fetch when present + fresh.
type Entry struct {
	URL           string
	FinalURL      string
	StatusCode    int
	ContentType   string
	ContentLength int
	Headers       map[string]any
	Body          []byte
	BodySize      int
	RedirectChain []Hop
	FetchedAt     time.Time
	ETag          string
	LastModified  string
	// BodySHA256 is the SHA-256 of Body. Computed on Upsert; populated
	// on Lookup so callers comparing two entries don't have to rehash.
	// Nil for rows written before migration 0003 (organic backfill on
	// next fetch). Length 32 when set.
	BodySHA256 []byte
}

// UpsertResult reports whether the body changed since the prior write
// for this URL. Callers use it to emit asset_event "url_body_changed"
// or skip a downstream re-analysis. First-write behavior:
// PreviousSHA256 nil, Changed false — establishes the baseline.
type UpsertResult struct {
	PreviousSHA256 []byte // nil on first fetch
	Changed        bool   // true when prior existed and bytes differ
	SizeDelta      int    // new BodySize - prior body_size
}

// Cache wraps the PG-backed body cache with a simple Lookup/Upsert
// surface. Workers stash it on their Tool struct and call Lookup
// before issuing a network fetch + Upsert on success.
type Cache struct {
	pool *pgxpool.Pool
	mu   sync.RWMutex
	// staleAfter — lookups returning a row older than this are
	// treated as misses so the caller refetches. Operators tune down
	// to tighten freshness, up to favor network economy.
	//
	// Read via staleAfterRead(); written via SetStaleAfter (the
	// runtime calls it on cluster_settings_changed NOTIFY so an
	// operator edit applies on the next Lookup).
	staleAfter time.Duration
}

// staleAfterRead is the lock-protected accessor; Lookup uses it
// rather than the field directly so SetStaleAfter swaps are visible
// without races.
func (c *Cache) staleAfterRead() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.staleAfter
}

// SetStaleAfter rewrites the freshness threshold. The runtime's
// cluster_settings listener calls this so an operator edit takes
// effect immediately rather than waiting for a worker restart.
// Zero or negative disables the staleness check (every cached row
// counts as fresh — useful for forensic mode).
func (c *Cache) SetStaleAfter(d time.Duration) {
	c.mu.Lock()
	c.staleAfter = d
	c.mu.Unlock()
}

// StaleAfter is the legacy field-style accessor used by external
// code that pre-dated SetStaleAfter. Kept for back-compat; new code
// reads via the dedicated method or stays inside the package.
func (c *Cache) StaleAfter() time.Duration { return c.staleAfterRead() }

// Per-process counters. Expose as Prom metrics via the worker-sdk
// metrics package — incremented on every Lookup / Upsert path so
// the operator can see "between waves we hit 95% on the body cache".
var (
	cntLookupHit   = prometheus.NewCounter(prometheus.CounterOpts{Name: "reconmesh_httpcache_lookups_hit_total", Help: "Body-cache lookups that returned a fresh entry."})
	cntLookupMiss  = prometheus.NewCounter(prometheus.CounterOpts{Name: "reconmesh_httpcache_lookups_miss_total", Help: "Body-cache lookups with no row, an expired row, or a backend error."})
	cntLookupStale = prometheus.NewCounter(prometheus.CounterOpts{Name: "reconmesh_httpcache_lookups_stale_total", Help: "Body-cache lookups that found a row past StaleAfter (treated as a miss)."})
	cntUpsert      = prometheus.NewCounter(prometheus.CounterOpts{Name: "reconmesh_httpcache_upserts_total", Help: "Body-cache writes (one per cached response)."})
	cntBodyChanged = prometheus.NewCounter(prometheus.CounterOpts{Name: "reconmesh_httpcache_body_changed_total", Help: "Upserts where the new body sha256 differed from the prior — drives Phase 3 url_body_changed asset events."})
)

func init() {
	prometheus.MustRegister(cntLookupHit, cntLookupMiss, cntLookupStale, cntUpsert, cntBodyChanged)
}

// New opens (or reuses) a pgxpool against dsn. Caller owns the
// resulting Close().
func New(ctx context.Context, dsn string) (*Cache, error) {
	if dsn == "" {
		return nil, errors.New("httpcache: PG DSN required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("httpcache: pool: %w", err)
	}
	return &Cache{pool: pool, staleAfter: 24 * time.Hour}, nil
}

// FromPool wraps an existing pgxpool. Use this when the caller
// already has a pool open (e.g. the worker-sdk runtime's pool).
func FromPool(pool *pgxpool.Pool) *Cache {
	return &Cache{pool: pool, staleAfter: 24 * time.Hour}
}

// Pool exposes the underlying pgxpool so sister caches that share the
// same Postgres (e.g. SourceCache for tm_extracted_sources) can attach
// without opening a duplicate connection set. Caller MUST NOT Close()
// this pool · ownership stays with the original Cache.
func (c *Cache) Pool() *pgxpool.Pool {
	return c.pool
}

// FollowClusterSettings opens a LISTEN on cluster_settings_changed
// and updates StaleAfter from http_cache_ttl_hours whenever the
// operator edits the cluster settings. Returns immediately; the
// listener runs until ctx cancels.
//
// Workers that want their body cache freshness to track the cluster
// admin panel call this once at boot:
//
//	cache, _ := httpcache.New(ctx, dsn)
//	go cache.FollowClusterSettings(ctx)
//
// Best-effort: a transient PG error pauses the listener for 2s and
// retries. The cache keeps its current StaleAfter in the meantime.
func (c *Cache) FollowClusterSettings(ctx context.Context) {
	// Initial read so a fresh boot picks up the current value
	// before the first NOTIFY.
	c.refreshStaleFromCluster(ctx)
	for {
		conn, err := c.pool.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(2 * time.Second)
			continue
		}
		if _, err := conn.Exec(ctx, `LISTEN cluster_settings_changed`); err != nil {
			conn.Release()
			if ctx.Err() != nil {
				return
			}
			time.Sleep(2 * time.Second)
			continue
		}
		for {
			if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
				conn.Release()
				if ctx.Err() != nil {
					return
				}
				time.Sleep(2 * time.Second)
				break
			}
			c.refreshStaleFromCluster(ctx)
		}
	}
}

func (c *Cache) refreshStaleFromCluster(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var raw []byte
	err := c.pool.QueryRow(rctx,
		`SELECT settings FROM cluster_settings WHERE key = 'global'`).Scan(&raw)
	if err != nil {
		return
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		return
	}
	if v, ok := s["http_cache_ttl_hours"].(float64); ok && v > 0 {
		c.SetStaleAfter(time.Duration(v * float64(time.Hour)))
	}
}

// Close releases the pool. Safe to call multiple times. No-op when
// the pool was provided externally via FromPool — caller owns it.
func (c *Cache) Close() {
	if c.pool != nil {
		c.pool.Close()
	}
}

// Lookup returns the cached entry for rawURL, or (nil, nil) on miss
// or when the row is past StaleAfter. err is non-nil only on backend
// failure (DB down, etc.).
//
// We don't differentiate "absent" from "stale" in the return value
// because callers always treat both as miss. They'll Upsert after
// the network fetch, overwriting either case identically.
func (c *Cache) Lookup(ctx context.Context, rawURL string) (*Entry, error) {
	hash := urlHash(rawURL)
	const q = `
		SELECT url, final_url, status_code, content_type, content_length,
		       headers, body, body_size, redirect_chain,
		       fetched_at, etag, last_modified, body_sha256
		  FROM tm_http_bodies
		 WHERE url_hash = $1`
	var (
		e         Entry
		rawHeader []byte
		rawChain  []byte
		etag      *string
		lm        *string
		ct        *string
		fu        *string
		sha       []byte
	)
	err := c.pool.QueryRow(ctx, q, hash).Scan(
		&e.URL, &fu, &e.StatusCode, &ct, &e.ContentLength,
		&rawHeader, &e.Body, &e.BodySize, &rawChain,
		&e.FetchedAt, &etag, &lm, &sha,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		cntLookupMiss.Inc()
		return nil, nil
	}
	if err != nil {
		cntLookupMiss.Inc()
		return nil, err
	}
	stale := c.staleAfterRead()
	if stale > 0 && time.Since(e.FetchedAt) > stale {
		cntLookupStale.Inc()
		return nil, nil
	}
	cntLookupHit.Inc()
	if fu != nil {
		e.FinalURL = *fu
	}
	if ct != nil {
		e.ContentType = *ct
	}
	if etag != nil {
		e.ETag = *etag
	}
	if lm != nil {
		e.LastModified = *lm
	}
	if len(rawHeader) > 0 {
		_ = json.Unmarshal(rawHeader, &e.Headers)
	}
	if len(rawChain) > 0 {
		_ = json.Unmarshal(rawChain, &e.RedirectChain)
	}
	if len(sha) > 0 {
		e.BodySHA256 = sha
	}
	return &e, nil
}

// Upsert writes (or refreshes) an entry. Idempotent on the url_hash
// PK — re-fetching the same URL within a wave overwrites the prior
// row, including fetched_at so freshness rolls forward.
func (c *Cache) Upsert(ctx context.Context, e *Entry) (*UpsertResult, error) {
	if e == nil || e.URL == "" {
		return nil, errors.New("httpcache: entry.URL required")
	}
	hash := urlHash(e.URL)
	headerJSON, err := json.Marshal(e.Headers)
	if err != nil {
		return nil, fmt.Errorf("httpcache: headers: %w", err)
	}
	chainJSON, err := json.Marshal(e.RedirectChain)
	if err != nil {
		return nil, fmt.Errorf("httpcache: chain: %w", err)
	}
	bodySize := e.BodySize
	if bodySize == 0 {
		bodySize = len(e.Body)
	}
	// Compute SHA on the canonicalized body bytes. This is what Phase 3
	// uses to detect change between fetches; the column is also indexed
	// (partial) so future "find every URL whose body matched X" queries
	// stay cheap.
	sum := sha256.Sum256(e.Body)
	newSHA := sum[:]
	e.BodySHA256 = newSHA

	res := &UpsertResult{}

	// One transaction so the history INSERT and the bodies UPSERT are
	// atomic — a crash mid-flow either leaves the prior state intact
	// or commits the whole change.
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("httpcache: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // safe after Commit

	// Read the prior row (if any) for the diff. We snapshot its
	// payload columns so the history INSERT can capture them WITHOUT
	// duplicating the latest-row body — the history row stores the
	// PRIOR content, not the new one.
	var (
		priorSHA       []byte
		priorBody      []byte
		priorSize      int
		priorStatus    int
		priorCT        *string
		priorHeaders   []byte
		priorFetchedAt time.Time
		hasPrior       bool
	)
	err = tx.QueryRow(ctx, `
		SELECT body_sha256, body, body_size, status_code,
		       content_type, headers, fetched_at
		  FROM tm_http_bodies
		 WHERE url_hash = $1`, hash).Scan(
		&priorSHA, &priorBody, &priorSize, &priorStatus,
		&priorCT, &priorHeaders, &priorFetchedAt,
	)
	switch {
	case err == nil:
		hasPrior = true
	case errors.Is(err, pgx.ErrNoRows):
		// First fetch — fall through.
	default:
		return nil, fmt.Errorf("httpcache: prior lookup: %w", err)
	}

	// Decide whether the body changed. We treat "prior had no SHA"
	// (pre-migration row) as "unknown" — same body bytes are treated
	// as unchanged, but if priorSHA is nil we compute it inline so
	// the comparison still works.
	if hasPrior {
		if len(priorSHA) == 0 {
			ps := sha256.Sum256(priorBody)
			priorSHA = ps[:]
		}
		if !bytesEqual(priorSHA, newSHA) {
			res.Changed = true
			res.PreviousSHA256 = priorSHA
			res.SizeDelta = bodySize - priorSize
			// Snapshot the prior version into history. Trigger
			// `tm_http_body_history_trim_trg` enforces the 3-row cap.
			ct := ""
			if priorCT != nil {
				ct = *priorCT
			}
			if _, herr := tx.Exec(ctx, `
				INSERT INTO tm_http_body_history
				    (url_hash, fetched_at, body, body_size, body_sha256,
				     status_code, content_type, headers)
				VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8::jsonb)
				ON CONFLICT (url_hash, fetched_at) DO NOTHING`,
				hash, priorFetchedAt, priorBody, priorSize, priorSHA,
				priorStatus, ct, priorHeaders,
			); herr != nil {
				return nil, fmt.Errorf("httpcache: history insert: %w", herr)
			}
		}
	}

	const upsert = `
		INSERT INTO tm_http_bodies
		    (url_hash, url, final_url, status_code, content_type,
		     content_length, headers, body, body_size, redirect_chain,
		     fetched_at, etag, last_modified, body_sha256)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10::jsonb,
		        NOW(), NULLIF($11, ''), NULLIF($12, ''), $13)
		ON CONFLICT (url_hash) DO UPDATE
		   SET url            = EXCLUDED.url,
		       final_url      = EXCLUDED.final_url,
		       status_code    = EXCLUDED.status_code,
		       content_type   = EXCLUDED.content_type,
		       content_length = EXCLUDED.content_length,
		       headers        = EXCLUDED.headers,
		       body           = EXCLUDED.body,
		       body_size      = EXCLUDED.body_size,
		       redirect_chain = EXCLUDED.redirect_chain,
		       fetched_at     = NOW(),
		       etag           = EXCLUDED.etag,
		       last_modified  = EXCLUDED.last_modified,
		       body_sha256    = EXCLUDED.body_sha256`
	_, err = tx.Exec(ctx, upsert,
		hash, e.URL, e.FinalURL, e.StatusCode, e.ContentType,
		e.ContentLength, string(headerJSON), e.Body, bodySize, string(chainJSON),
		e.ETag, e.LastModified, newSHA,
	)
	if err != nil {
		return nil, fmt.Errorf("httpcache: upsert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("httpcache: commit: %w", err)
	}
	cntUpsert.Inc()
	if res.Changed {
		cntBodyChanged.Inc()
	}
	return res, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// urlHash canonicalizes the URL (lowercase scheme+host, drop default
// ports, normalize path) and returns its SHA-256 digest. The
// canonicalization mirrors what most servers' caches use so cross-
// worker hits are stable.
func urlHash(rawURL string) []byte {
	canon := canonicalize(rawURL)
	sum := sha256.Sum256([]byte(canon))
	return sum[:]
}

func canonicalize(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return rawURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	// Strip default ports — most servers respond identically on the
	// implicit and explicit forms. Hash collisions otherwise.
	if (u.Scheme == "http" && strings.HasSuffix(u.Host, ":80")) ||
		(u.Scheme == "https" && strings.HasSuffix(u.Host, ":443")) {
		u.Host = strings.TrimSuffix(strings.TrimSuffix(u.Host, ":80"), ":443")
	}
	if u.Path == "" {
		u.Path = "/"
	}
	// Drop fragment — never sent to the server.
	u.Fragment = ""
	return u.String()
}
