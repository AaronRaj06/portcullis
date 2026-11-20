package proxy

import (
	"context"
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

// writeSelfSigned writes a throwaway cert/key for 127.0.0.1 and returns their paths and a pool trusting it.
func writeSelfSigned(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	return writeCert(t, t.TempDir(), 1)
}

// writeCert writes a self-signed cert with the given serial into dir (overwriting).
func writeCert(t *testing.T, dir string, serial int64) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "portcullis-test"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(c)
	return certFile, keyFile, pool
}

func TestTLSTermination(t *testing.T) {
	certFile, keyFile, pool := writeSelfSigned(t)
	cfg, err := TLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "secure") }), DefaultLimits, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, ln, time.Second) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	addr := ln.Addr().String()

	t.Run("modern client gets TLS 1.3 with post-quantum hybrid key exchange", func(t *testing.T) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		st := conn.ConnectionState()
		if st.Version != tls.VersionTLS13 {
			t.Errorf("version = %x, want TLS 1.3", st.Version)
		}
		if st.CurveID != tls.X25519MLKEM768 {
			t.Errorf("key exchange = %v, want X25519MLKEM768", st.CurveID)
		}
	})

	t.Run("TLS 1.1 is refused", func(t *testing.T) {
		conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11}) //nolint:gosec // deliberately old client
		if err == nil {
			_ = conn.Close()
			t.Fatal("TLS 1.1 handshake succeeded, want failure")
		}
	})

	t.Run("HTTPS request works", func(t *testing.T) {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
		resp, err := c.Get("https://" + addr + "/")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if string(b) != "secure" {
			t.Fatalf("body = %q", b)
		}
	})
}

func TestCertificateRotation(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, pool1 := writeCert(t, dir, 1)
	r, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), DefaultLimits, TLSConfigFrom(r))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, ln, time.Second) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()

	serial := func(pool *x509.CertPool) int64 {
		t.Helper()
		conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
	}
	if got := serial(pool1); got != 1 {
		t.Fatalf("serving serial %d, want 1", got)
	}
	_, _, pool2 := writeCert(t, dir, 2)
	if err := r.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := serial(pool2); got != 2 {
		t.Fatalf("after reload serving serial %d, want 2", got)
	}
	if err := os.WriteFile(keyFile, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Reload(); err == nil {
		t.Fatal("reload of a broken key succeeded")
	}
	if got := serial(pool2); got != 2 {
		t.Fatalf("a failed reload changed the certificate (serial %d)", got)
	}
}
