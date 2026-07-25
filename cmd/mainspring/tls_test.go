package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSignedCert generates a self-signed cert/key for 127.0.0.1 into dir
// and returns their paths plus the PEM cert bytes (for the client trust pool).
func writeSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string, certPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Unix(1700000000, 0),
		NotAfter:              time.Unix(1900000000, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, certPEM
}

// TestServeTLSHandshake exercises the exact TLS setup runServe uses: an
// http.Server with MinVersion TLS 1.2 serving via ListenAndServeTLS. It confirms
// an HTTPS request succeeds and that a TLS 1.1-max client is rejected.
func TestServeTLSHandshake(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, certPEM := writeSelfSignedCert(t, dir)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	srv := &http.Server{
		Handler:   mux,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}, // same as runServe
	}
	go func() { _ = srv.ServeTLS(ln, certPath, keyPath) }()
	defer srv.Close()

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to load test cert")
	}

	// TLS 1.2 client succeeds.
	okClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}}
	resp, err := okClient.Get("https://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("TLS 1.2 request should succeed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("unexpected body over HTTPS: %s", body)
	}

	// TLS 1.1-max client must be rejected by the MinVersion 1.2 server.
	oldClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MaxVersion: tls.VersionTLS11},
	}}
	if _, err := oldClient.Get("https://" + addr + "/healthz"); err == nil {
		t.Fatal("TLS 1.1 client should be rejected by a TLS 1.2-minimum server")
	}
}
