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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
}

// Cache wraps the PG-backed body cache with a simple Lookup/Upsert
// surface. Workers stash it on their Tool struct and call Lookup
// before issuing a network fetch + Upsert on success.
type Cache struct {
	pool *pgxpool.Pool
	// Stale-after threshold. Lookups returning a row older than this
	// are treated as misses so the caller refetches. Operators tune
	// down to tighten freshness, up to favor network economy.
	StaleAfter time.Duration
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
	return &Cache{pool: pool, StaleAfter: 24 * time.Hour}, nil
}

// FromPool wraps an existing pgxpool. Use this when the caller
// already has a pool open (e.g. the worker-sdk runtime's pool).
func FromPool(pool *pgxpool.Pool) *Cache {
	return &Cache{pool: pool, StaleAfter: 24 * time.Hour}
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
		       fetched_at, etag, last_modified
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
	)
	err := c.pool.QueryRow(ctx, q, hash).Scan(
		&e.URL, &fu, &e.StatusCode, &ct, &e.ContentLength,
		&rawHeader, &e.Body, &e.BodySize, &rawChain,
		&e.FetchedAt, &etag, &lm,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if c.StaleAfter > 0 && time.Since(e.FetchedAt) > c.StaleAfter {
		return nil, nil
	}
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
	return &e, nil
}

// Upsert writes (or refreshes) an entry. Idempotent on the url_hash
// PK — re-fetching the same URL within a wave overwrites the prior
// row, including fetched_at so freshness rolls forward.
func (c *Cache) Upsert(ctx context.Context, e *Entry) error {
	if e == nil || e.URL == "" {
		return errors.New("httpcache: entry.URL required")
	}
	hash := urlHash(e.URL)
	headerJSON, err := json.Marshal(e.Headers)
	if err != nil {
		return fmt.Errorf("httpcache: headers: %w", err)
	}
	chainJSON, err := json.Marshal(e.RedirectChain)
	if err != nil {
		return fmt.Errorf("httpcache: chain: %w", err)
	}
	bodySize := e.BodySize
	if bodySize == 0 {
		bodySize = len(e.Body)
	}
	const upsert = `
		INSERT INTO tm_http_bodies
		    (url_hash, url, final_url, status_code, content_type,
		     content_length, headers, body, body_size, redirect_chain,
		     fetched_at, etag, last_modified)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10::jsonb,
		        NOW(), NULLIF($11, ''), NULLIF($12, ''))
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
		       last_modified  = EXCLUDED.last_modified`
	_, err = c.pool.Exec(ctx, upsert,
		hash, e.URL, e.FinalURL, e.StatusCode, e.ContentType,
		e.ContentLength, string(headerJSON), e.Body, bodySize, string(chainJSON),
		e.ETag, e.LastModified,
	)
	return err
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
