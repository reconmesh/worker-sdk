package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DiscoveredRecord is one row tm-subfind (or any future enumeration
// worker) wants to push into the dns-service cache. Mirrors the
// JSON shape accepted by `POST /v1/records/bulk` — see
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
// lost forever — the records will be re-discovered on the next
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

	cli := &http.Client{Timeout: 30 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("dns push: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("dns push: HTTP %d", resp.StatusCode)
	}
	return nil
}
