//go:build integration

// Real-PG integration test for Upsert's diff + history semantics.
// Boots a Postgres testcontainer, applies the platform schema +
// migration 0003, then exercises:
//
//   1. First Upsert → res.Changed=false, no history.
//   2. Same body Upsert → res.Changed=false, no history added.
//   3. Different body Upsert → res.Changed=true, prior body lands
//      in tm_http_body_history.
//   4. Five Upserts with rotating bodies → trigger trims history to
//      the latest 3 versions per URL.
//
// Skipped by default - `go test -tags=integration ./...`.
package httpcache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func startPG(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pgC, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("recon"),
		tcpostgres.WithUsername("recon"),
		tcpostgres.WithPassword("recon"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(45*time.Second)),
	)
	if err != nil {
		t.Fatalf("pg start: %v", err)
	}
	t.Cleanup(func() { _ = pgC.Terminate(ctx) })
	dsn, err := pgC.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, p := range []string{
		"../../../recon-platform/migrations/0001_init.up.sql",
		"../../../recon-platform/migrations/0003_body_history.up.sql",
	} {
		schema, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if _, err := pool.Exec(ctx, string(schema)); err != nil {
			t.Fatalf("apply %s: %v", p, err)
		}
	}
	return pool
}

func TestUpsert_FirstFetch_NoHistory(t *testing.T) {
	pool := startPG(t)
	c := FromPool(pool)
	ctx := context.Background()
	body := []byte("hello world")

	res, err := c.Upsert(ctx, &Entry{
		URL: "https://x.example.com/", StatusCode: 200,
		ContentType: "text/plain", Body: body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed {
		t.Error("first Upsert: Changed should be false")
	}
	if res.PreviousSHA256 != nil {
		t.Errorf("first Upsert: PreviousSHA256 should be nil, got %x", res.PreviousSHA256)
	}

	// History table empty for this URL.
	var n int
	hash := urlHash("https://x.example.com/")
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tm_http_body_history WHERE url_hash = $1`, hash).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("history rows = %d, want 0 on first fetch", n)
	}

	// Lookup surfaces the SHA.
	got, err := c.Lookup(ctx, "https://x.example.com/")
	if err != nil || got == nil {
		t.Fatalf("lookup: err=%v entry=%v", err, got)
	}
	want := sha256.Sum256(body)
	if !bytesEqual(got.BodySHA256, want[:]) {
		t.Errorf("BodySHA256 = %x, want %x", got.BodySHA256, want[:])
	}
}

func TestUpsert_SameBody_NoHistoryNoChange(t *testing.T) {
	pool := startPG(t)
	c := FromPool(pool)
	ctx := context.Background()
	body := []byte("stable content")

	for i := 0; i < 3; i++ {
		res, err := c.Upsert(ctx, &Entry{
			URL: "https://x.example.com/", StatusCode: 200, Body: body,
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Changed {
			t.Errorf("iteration %d: Changed should be false on identical bodies", i)
		}
	}

	hash := urlHash("https://x.example.com/")
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tm_http_body_history WHERE url_hash = $1`, hash).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("history rows = %d, want 0 (no change)", n)
	}
}

func TestUpsert_BodyChange_HistoryRecorded(t *testing.T) {
	pool := startPG(t)
	c := FromPool(pool)
	ctx := context.Background()

	// First write - establishes baseline.
	if _, err := c.Upsert(ctx, &Entry{
		URL: "https://x.example.com/", StatusCode: 200,
		ContentType: "application/javascript",
		Body:        []byte("v1 body"),
	}); err != nil {
		t.Fatal(err)
	}

	// Second write - body differs.
	res, err := c.Upsert(ctx, &Entry{
		URL: "https://x.example.com/", StatusCode: 200,
		ContentType: "application/javascript",
		Body:        []byte("v2 body has more"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("Changed should be true on differing body")
	}
	if len(res.PreviousSHA256) != sha256.Size {
		t.Errorf("PreviousSHA256 length = %d, want %d", len(res.PreviousSHA256), sha256.Size)
	}
	wantPrior := sha256.Sum256([]byte("v1 body"))
	if !bytesEqual(res.PreviousSHA256, wantPrior[:]) {
		t.Errorf("PreviousSHA256 mismatch")
	}
	if res.SizeDelta != len("v2 body has more")-len("v1 body") {
		t.Errorf("SizeDelta = %d, want %d", res.SizeDelta, len("v2 body has more")-len("v1 body"))
	}

	// History has exactly one row carrying the prior body.
	hash := urlHash("https://x.example.com/")
	var (
		n        int
		histBody []byte
	)
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tm_http_body_history WHERE url_hash = $1`, hash).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("history rows = %d, want 1 after one change", n)
	}
	if err := pool.QueryRow(ctx,
		`SELECT body FROM tm_http_body_history WHERE url_hash = $1`, hash).Scan(&histBody); err != nil {
		t.Fatal(err)
	}
	if string(histBody) != "v1 body" {
		t.Errorf("history body = %q, want %q", histBody, "v1 body")
	}
}

// Trigger enforces 3-row max per URL - the operator-confirmed
// retention budget. Insert 5 distinct versions and verify only the
// 3 most-recent prior versions remain in history (the 4th = current
// body, in tm_http_bodies; the 1st is dropped).
func TestUpsert_HistoryRetention_3Versions(t *testing.T) {
	pool := startPG(t)
	c := FromPool(pool)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		body := []byte(fmt.Sprintf("v%d body content", i))
		if _, err := c.Upsert(ctx, &Entry{
			URL: "https://x.example.com/", StatusCode: 200, Body: body,
		}); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		// Spread fetched_at: the trigger orders by fetched_at DESC.
		// Without a small gap two history rows could land on the same
		// timestamp and the trim's ORDER BY would be ambiguous.
		time.Sleep(15 * time.Millisecond)
	}

	hash := urlHash("https://x.example.com/")
	rows, err := pool.Query(ctx,
		`SELECT body FROM tm_http_body_history
		 WHERE url_hash = $1 ORDER BY fetched_at DESC`, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var bodies []string
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(b))
	}
	if len(bodies) != 3 {
		t.Fatalf("history rows = %d, want 3 (retention cap)", len(bodies))
	}
	// Newest history row is the v3 prior (i.e. the one displaced by v4).
	want := []string{"v3 body content", "v2 body content", "v1 body content"}
	for i, b := range bodies {
		if b != want[i] {
			t.Errorf("history[%d] = %q, want %q", i, b, want[i])
		}
	}
}
