package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/reconmesh/worker-sdk/sdk/mtls"
)

// DiscoveredRecord is one row tm-subfind (or any future enumeration
// worker) wants to push into the dns-service cache. Mirrors the
// JSON shape accepted by `POST /v1/records/bulk` - see
// dns-service/internal/server/server.go::handleBulkUpsert.
type DiscoveredRecord struct {
	Host       string    `json:"host"`
	RType      string    `json:"rtype"`             // "A" | "AAAA" | "CNAME" | ...
	Answers    []string  `json:"answers"`
	TTL        int       `json:"ttl,omitempty"`
	ValidUntil time.Time `json:"valid_until,omitempty"`
	Upstream   string    `json:"upstream,omitempty"`
}

// PushRecords ships a batch of discovered records to dns-service so
// every other worker hits the cache on subsequent lookups instead
// of fanning out to upstream resolvers.
//
// Designed for tm-subfind which uses its own resolvers during massive
// passive-source enumeration (multi-source resilience), then writes
// back here at the end of the run. The same helper works for any
// worker that learns of (host → IP) mappings out-of-band.
//
// Endpoint base default = http://dns-service:7080 (the docker-compose
// service name). Override via DNS_SERVICE_ADMIN_URL env var.
//
// On transport failure we return the error rather than swallow it
// so the worker can surface it in its run stats. The data isn't
// lost forever - the records will be re-discovered on the next
// scope run.
func PushRecords(ctx context.Context, baseURL string, records []DiscoveredRecord) error {
	if len(records) == 0 {
		return nil
	}
	if baseURL == "" {
		baseURL = "http://dns-service:7080"
	}
	body, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("dns push: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/v1/records/bulk", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := mtls.Client().Do(req)
	if err != nil {
		return fmt.Errorf("dns push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("dns push: HTTP %d", resp.StatusCode)
	}
	return nil
}

// BulkResolution is one entry in BulkResolve's response. CNAME may be
// empty when the host has no alias; A / AAAA may be empty when the
// host has no record of that type or the cache miss flowed through
// to a pool error captured in Error.
type BulkResolution struct {
	Host  string   `json:"host"`
	A     []string `json:"a,omitempty"`
	AAAA  []string `json:"aaaa,omitempty"`
	CNAME string   `json:"cname,omitempty"`
	Error string   `json:"error,omitempty"`
}

// BulkResolve fans out N hostname resolutions in one HTTP request,
// returning the parallel result set. dns-service does the work in
// parallel internally (32-way) so a 1000-host call typically finishes
// in single-digit seconds when most are cached.
//
// Pre-warming (caller ignores the returned slice): pass `types: nil`
// to populate every cache layer for the listed hosts before a heavy
// phase wave kicks off. The persisted answers serve subsequent worker
// queries as PG hits.
//
// Bulk lookup (caller uses the returned slice): pass the explicit
// types you need ("A", "AAAA", "CNAME") to avoid populating extras.
//
// Hard cap of 10000 hosts per request - chunk before calling on
// larger workloads.
func BulkResolve(ctx context.Context, baseURL string, hosts []string, types []string) ([]BulkResolution, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	if baseURL == "" {
		baseURL = "http://dns-service:7080"
	}
	body, err := json.Marshal(map[string]any{
		"hosts": hosts,
		"types": types,
	})
	if err != nil {
		return nil, fmt.Errorf("bulk resolve: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/v1/resolve/bulk", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := mtls.Client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("bulk resolve: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("bulk resolve: HTTP %d", resp.StatusCode)
	}
	var out []BulkResolution
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("bulk resolve: decode: %w", err)
	}
	return out, nil
}
