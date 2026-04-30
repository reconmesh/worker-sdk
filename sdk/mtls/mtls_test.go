package mtls

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// build() is the real load path; Client() wraps it in sync.Once which
// would fight the table tests. Verify each env-permutation behavior.

func TestBuild_NoEnv_ReturnsDefault(t *testing.T) {
	t.Setenv("PARABELLUM_TLS_CA", "")
	t.Setenv("PARABELLUM_TLS_CERT", "")
	t.Setenv("PARABELLUM_TLS_KEY", "")
	c, err := build()
	if err != nil {
		t.Fatalf("build with no env: %v", err)
	}
	if c != http.DefaultClient {
		t.Fatal("expected DefaultClient when no env set")
	}
}

func TestBuild_PartialEnv_Errors(t *testing.T) {
	cases := []map[string]string{
		{"PARABELLUM_TLS_CA": "/tmp/ca.crt"},
		{"PARABELLUM_TLS_CA": "/tmp/ca.crt", "PARABELLUM_TLS_CERT": "/tmp/c.crt"},
		{"PARABELLUM_TLS_CERT": "/tmp/c.crt"},
		{"PARABELLUM_TLS_KEY": "/tmp/c.key"},
	}
	for i, env := range cases {
		t.Setenv("PARABELLUM_TLS_CA", env["PARABELLUM_TLS_CA"])
		t.Setenv("PARABELLUM_TLS_CERT", env["PARABELLUM_TLS_CERT"])
		t.Setenv("PARABELLUM_TLS_KEY", env["PARABELLUM_TLS_KEY"])
		if _, err := build(); err == nil {
			t.Errorf("case %d: build with partial env should error, got nil", i)
		}
	}
}

func TestBuild_InvalidCAFile_Errors(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "bad-ca.crt")
	if err := os.WriteFile(caPath, []byte("not a pem"), 0600); err != nil {
		t.Fatal(err)
	}
	cert, key := writeSelfSignedPair(t, dir)
	t.Setenv("PARABELLUM_TLS_CA", caPath)
	t.Setenv("PARABELLUM_TLS_CERT", cert)
	t.Setenv("PARABELLUM_TLS_KEY", key)
	if _, err := build(); err == nil {
		t.Fatal("expected error for non-PEM CA file")
	}
}

func TestBuild_ValidPair_ReturnsConfiguredClient(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeSelfSignedPair(t, dir)
	// Reuse the cert as its own CA root for the test.
	t.Setenv("PARABELLUM_TLS_CA", cert)
	t.Setenv("PARABELLUM_TLS_CERT", cert)
	t.Setenv("PARABELLUM_TLS_KEY", key)
	c, err := build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if c == http.DefaultClient {
		t.Fatal("got DefaultClient, expected configured Client")
	}
	if c.Timeout == 0 {
		t.Error("expected non-zero Timeout on configured Client")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", c.Transport)
	}
	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil")
	}
	if tr.TLSClientConfig.MinVersion < 0x0303 { // TLS 1.2
		t.Errorf("MinVersion = %x, want ≥ TLS 1.2", tr.TLSClientConfig.MinVersion)
	}
	if len(tr.TLSClientConfig.Certificates) != 1 {
		t.Errorf("Certificates count = %d, want 1", len(tr.TLSClientConfig.Certificates))
	}
	if tr.TLSClientConfig.RootCAs == nil {
		t.Error("RootCAs is nil")
	}
}

// writeSelfSignedPair generates an RSA-2048 self-signed cert+key pair
// in dir and returns the file paths. Good enough for build() - we
// don't actually open a TLS connection here.
func writeSelfSignedPair(t *testing.T, dir string) (certPath, keyPath string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IsCA:         true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	cf, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	cf.Close()
	kf, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	keyDER := x509.MarshalPKCS1PrivateKey(priv)
	if err := pem.Encode(kf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER}); err != nil {
		t.Fatal(err)
	}
	kf.Close()
	return
}
