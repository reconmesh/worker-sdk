// Package mtls — small helper for workers that talk to internal
// HTTPS endpoints (controlplane / dns-service) over mTLS.
//
// The worker-sdk doesn't FORCE mTLS — when the env vars are unset
// the helper returns http.DefaultClient. Operators who deploy with
// gen-certs.sh export:
//
//	RECONMESH_TLS_CA   /etc/reconmesh/tls/ca.crt
//	RECONMESH_TLS_CERT /etc/reconmesh/tls/worker.crt
//	RECONMESH_TLS_KEY  /etc/reconmesh/tls/worker.key
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
// — caller doesn't need to special-case.
func Client() *http.Client {
	clientOnce.Do(func() {
		c, err := build()
		if err != nil {
			// Log to stderr by default — workers using this helper
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
	caPath := os.Getenv("RECONMESH_TLS_CA")
	certPath := os.Getenv("RECONMESH_TLS_CERT")
	keyPath := os.Getenv("RECONMESH_TLS_KEY")
	if caPath == "" && certPath == "" && keyPath == "" {
		// No TLS configured — caller gets the stdlib default. Keeps
		// dev environments and behind-proxy deployments simple.
		return http.DefaultClient, nil
	}
	if caPath == "" || certPath == "" || keyPath == "" {
		return nil, errors.New("mtls: set all of RECONMESH_TLS_{CA,CERT,KEY} or none")
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
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				RootCAs:      pool,
				Certificates: []tls.Certificate{pair},
			},
			MaxIdleConns:        20,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}, nil
}
