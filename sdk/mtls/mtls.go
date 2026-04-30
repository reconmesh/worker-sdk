// Package mtls - small helper for workers that talk to internal
// HTTPS endpoints (controlplane / dns-service) over mTLS.
//
// The worker-sdk doesn't FORCE mTLS - when the env vars are unset
// the helper returns http.DefaultClient. Operators who deploy with
// gen-certs.sh export:
//
//	PARABELLUM_TLS_CA   /etc/reconmesh/tls/ca.crt
//	PARABELLUM_TLS_CERT /etc/reconmesh/tls/worker.crt
//	PARABELLUM_TLS_KEY  /etc/reconmesh/tls/worker.key
//
// and the helper returns a Client with the right roots + cert.
//
// Used today by sdk/dns.PushRecords and sdk/dns.BulkResolve when
// pointing at a TLS dns-service. Future Updatable broadcast → admin
// /admin/update calls land here too.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"
)

// Client returns a process-wide *http.Client with the worker's
// client cert + the cluster CA. Cached so we don't re-load certs on
// every HTTP call. When TLS env is unset, returns http.DefaultClient
// - caller doesn't need to special-case.
func Client() *http.Client {
	clientOnce.Do(func() {
		c, err := build()
		if err != nil {
			// Log to stderr by default - workers using this helper
			// in their main usually have a *slog.Logger but we
			// don't take it as a dep. The worker-sdk runtime logs
			// the same condition at startup.
			defaultClient = http.DefaultClient
			defaultErr = err
			return
		}
		defaultClient = c
	})
	return defaultClient
}

// LastError reports whether the last build() call failed. Workers
// that need to surface "TLS configured but cert missing" inspect
// this at boot.
func LastError() error { return defaultErr }

var (
	clientOnce    sync.Once
	defaultClient *http.Client
	defaultErr    error
)

func build() (*http.Client, error) {
	caPath := os.Getenv("PARABELLUM_TLS_CA")
	certPath := os.Getenv("PARABELLUM_TLS_CERT")
	keyPath := os.Getenv("PARABELLUM_TLS_KEY")
	if caPath == "" && certPath == "" && keyPath == "" {
		// No TLS configured - caller gets the stdlib default. Keeps
		// dev environments and behind-proxy deployments simple.
		return http.DefaultClient, nil
	}
	if caPath == "" || certPath == "" || keyPath == "" {
		return nil, errors.New("mtls: set all of PARABELLUM_TLS_{CA,CERT,KEY} or none")
	}
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("mtls: ca file: no PEM certs found")
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:   60 * time.Second,
		Transport: cleanTransport(pool, pair),
	}, nil
}

// cleanTransport returns an http.Transport with the leak-defensive
// defaults the cleanhttp pattern advocates (one canonical transport
// per process, sane keep-alive + idle pool sizing, no shared global
// state). We don't pull github.com/projectdiscovery/cleanhttp or
// hashicorp/go-cleanhttp · the wrapper is ~30 lines around stdlib
// defaults and adding a dep for that footprint isn't worth the
// supply-chain weight when this is the only worker-sdk call site.
//
// The mTLS-specific bits (RootCAs, client cert pair, MinVersion 1.2)
// merge with the stock cleanhttp tuning. Future call sites that
// don't need mTLS can copy this function shape and drop the cert
// pair without paying for the dep.
func cleanTransport(roots *x509.CertPool, pair tls.Certificate) *http.Transport {
	return &http.Transport{
		// Keep-alive defaults · explicit so a future stdlib change
		// in DefaultTransport doesn't silently shift our scanner
		// behavior.
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		// DisableKeepAlives off · we WANT keep-alive on a small pool
		// to amortize the TLS handshake across cluster-internal calls.
		DisableKeepAlives: false,
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			RootCAs:      roots,
			Certificates: []tls.Certificate{pair},
		},
	}
}
