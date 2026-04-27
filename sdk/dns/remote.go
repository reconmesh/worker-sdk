package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// HTTPBackend talks to dns-service over HTTP/JSON. We deliberately
// pick HTTP/JSON over gRPC for the first implementation:
//
//   - one transport, no protoc tooling for tool authors
//   - browsable via curl + Adminer-style UIs in dev
//   - upgrading to gRPC later only changes this file + dns-service's
//     listener; the Resolver interface stays
//
// Endpoint shape (versioned, /v1 mounted by dns-service):
//
//   GET  /v1/resolve?host=x         → {"a":[...],"aaaa":[...],...}
//   GET  /v1/resolve-all?host=x     → full Records
//   GET  /v1/cname?host=x
//   GET  /v1/mx?host=x
//   GET  /v1/txt?host=x
//   GET  /v1/ns?host=x
//
// Errors are surfaced via HTTP status + a JSON {"error":"..."} body.
// 404 + {"error":"nxdomain"} maps to ErrNXDomain; 504 maps to
// ErrTimeout; everything else is wrapped verbatim.
type HTTPBackend struct {
	// BaseURL of the dns-service. Trailing slash optional.
	BaseURL string
	// HTTPClient used for requests. nil → http.DefaultClient with a
	// 10s timeout. Workers running in tight loops should pass a
	// keep-alive-tuned client.
	HTTPClient *http.Client
	// CallerTool is sent as X-Caller-Tool. The service uses it for
	// per-tool stats and rate-limiting if needed.
	CallerTool string
	// Fallback is consulted on transport errors. Typically a
	// LocalBackend so a flaky dns-service doesn't grind workers
	// to a halt. nil → no fallback (caller gets the raw error).
	Fallback Resolver
}

func (b *HTTPBackend) client() *http.Client {
	if b.HTTPClient != nil {
		return b.HTTPClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (b *HTTPBackend) endpoint(path, host string) string {
	base := b.BaseURL
	if len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	q := url.Values{}
	q.Set("host", host)
	return base + path + "?" + q.Encode()
}

// errResp matches the JSON shape every error body uses.
type errResp struct {
	Error string `json:"error"`
}

// do issues the GET, parses JSON into out, and maps service-side
// errors. Returns a sentinel error on transport / non-2xx so callers
// can branch into Fallback cleanly.
func (b *HTTPBackend) do(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if b.CallerTool != "" {
		req.Header.Set("X-Caller-Tool", b.CallerTool)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := b.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrServiceUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		var er errResp
		_ = json.NewDecoder(resp.Body).Decode(&er)
		if er.Error == "nxdomain" {
			return ErrNXDomain
		}
		return fmt.Errorf("dns-service: %s", er.Error)
	}
	if resp.StatusCode == http.StatusGatewayTimeout {
		return ErrTimeout
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("dns-service: HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// callOrFallback invokes fn against the service. If the service is
// unreachable (ErrServiceUnavailable family), and a Fallback is
// configured, it tries the fallback and marks the result Degraded
// where applicable.
func (b *HTTPBackend) callOrFallback(
	ctx context.Context,
	primary func() error,
	fallback func() error,
) error {
	err := primary()
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrServiceUnavailable) && b.Fallback != nil && fallback != nil {
		return fallback()
	}
	return err
}

// Resolve uses /v1/resolve and falls back to the local resolver on
// service unavailability.
func (b *HTTPBackend) Resolve(ctx context.Context, host string) ([]net.IP, error) {
	type respShape struct {
		A    []string `json:"a"`
		AAAA []string `json:"aaaa"`
	}
	var ips []net.IP
	primary := func() error {
		var r respShape
		if err := b.do(ctx, b.endpoint("/v1/resolve", host), &r); err != nil {
			return err
		}
		for _, s := range r.A {
			if ip := net.ParseIP(s); ip != nil {
				ips = append(ips, ip)
			}
		}
		for _, s := range r.AAAA {
			if ip := net.ParseIP(s); ip != nil {
				ips = append(ips, ip)
			}
		}
		return nil
	}
	fallback := func() error {
		var err error
		ips, err = b.Fallback.Resolve(ctx, host)
		return err
	}
	return ips, b.callOrFallback(ctx, primary, fallback)
}

// ResolveAll uses /v1/resolve-all. On service-down, falls back; the
// fallback's Records are tagged Degraded=true so callers can log it.
func (b *HTTPBackend) ResolveAll(ctx context.Context, host string) (*Records, error) {
	var rec *Records
	primary := func() error {
		rec = &Records{}
		return b.do(ctx, b.endpoint("/v1/resolve-all", host), rec)
	}
	fallback := func() error {
		var err error
		rec, err = b.Fallback.ResolveAll(ctx, host)
		if rec != nil {
			rec.Degraded = true
		}
		return err
	}
	return rec, b.callOrFallback(ctx, primary, fallback)
}

func (b *HTTPBackend) LookupCNAME(ctx context.Context, host string) (string, error) {
	type respShape struct {
		CNAME string `json:"cname"`
	}
	var out string
	primary := func() error {
		var r respShape
		if err := b.do(ctx, b.endpoint("/v1/cname", host), &r); err != nil {
			return err
		}
		out = r.CNAME
		return nil
	}
	fallback := func() error {
		var err error
		out, err = b.Fallback.LookupCNAME(ctx, host)
		return err
	}
	return out, b.callOrFallback(ctx, primary, fallback)
}

func (b *HTTPBackend) LookupMX(ctx context.Context, host string) ([]MX, error) {
	type respShape struct {
		MX []MX `json:"mx"`
	}
	var out []MX
	primary := func() error {
		var r respShape
		if err := b.do(ctx, b.endpoint("/v1/mx", host), &r); err != nil {
			return err
		}
		out = r.MX
		return nil
	}
	fallback := func() error {
		var err error
		out, err = b.Fallback.LookupMX(ctx, host)
		return err
	}
	return out, b.callOrFallback(ctx, primary, fallback)
}

func (b *HTTPBackend) LookupTXT(ctx context.Context, host string) ([]string, error) {
	type respShape struct {
		TXT []string `json:"txt"`
	}
	var out []string
	primary := func() error {
		var r respShape
		if err := b.do(ctx, b.endpoint("/v1/txt", host), &r); err != nil {
			return err
		}
		out = r.TXT
		return nil
	}
	fallback := func() error {
		var err error
		out, err = b.Fallback.LookupTXT(ctx, host)
		return err
	}
	return out, b.callOrFallback(ctx, primary, fallback)
}

func (b *HTTPBackend) LookupNS(ctx context.Context, host string) ([]string, error) {
	type respShape struct {
		NS []string `json:"ns"`
	}
	var out []string
	primary := func() error {
		var r respShape
		if err := b.do(ctx, b.endpoint("/v1/ns", host), &r); err != nil {
			return err
		}
		out = r.NS
		return nil
	}
	fallback := func() error {
		var err error
		out, err = b.Fallback.LookupNS(ctx, host)
		return err
	}
	return out, b.callOrFallback(ctx, primary, fallback)
}
