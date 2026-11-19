package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Limits holds every timeout and size limit. Zero means "no limit".
type Limits struct {
	ReadHeaderTimeout     time.Duration // the full request header must arrive within this (slowloris defence)
	IdleTimeout           time.Duration // how long a keep-alive connection may sit idle between requests
	WriteTimeout          time.Duration // how long a single write to the client may block
	BodyReadTimeout       time.Duration // the longest gap allowed between reads of the client body
	DialTimeout           time.Duration // TCP connect to the upstream
	ResponseHeaderTimeout time.Duration // upstream must send response headers within this after the request is sent
	MaxHeaderBytes        int
	MaxBodyBytes          int64
}

// DefaultLimits are conservative values suitable for an internet-facing proxy.
var DefaultLimits = Limits{
	ReadHeaderTimeout:     10 * time.Second,
	IdleTimeout:           120 * time.Second,
	WriteTimeout:          30 * time.Second,
	BodyReadTimeout:       30 * time.Second,
	DialTimeout:           5 * time.Second,
	ResponseHeaderTimeout: 30 * time.Second,
	MaxHeaderBytes:        64 << 10,
	MaxBodyBytes:          10 << 20,
}

// NewServer returns the client-facing server. ReadTimeout and WriteTimeout are left
// at zero on purpose: whole-request deadlines would cut off long uploads and
// streams, so the Handler applies per-read and per-write idle deadlines instead
// (ADR 0004). tlsCfg may be nil for plain HTTP.
func NewServer(h http.Handler, l Limits, tlsCfg *tls.Config) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: l.ReadHeaderTimeout,
		IdleTimeout:       l.IdleTimeout,
		MaxHeaderBytes:    l.MaxHeaderBytes,
		TLSConfig:         tlsCfg,
	}
}

// NewTransport returns the upstream-facing client transport.
func NewTransport(l Limits) *http.Transport {
	d := &net.Dialer{Timeout: l.DialTimeout, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 nil, // never route upstream traffic through HTTP_PROXY from the environment
		DialContext:           d.DialContext,
		ResponseHeaderTimeout: l.ResponseHeaderTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    true, // pass bytes through as-is; don't decompress what the upstream sent
		ForceAttemptHTTP2:     true, // HTTP/2 to TLS upstreams (needed because we set a custom DialContext)
	}
}

// Serve runs srv on ln until ctx is cancelled, then drains: it stops accepting
// new connections, lets in-flight requests finish for up to drain, and
// force-closes whatever is left.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, drain time.Duration) error {
	errc := make(chan error, 1)
	go func() {
		if srv.TLSConfig != nil {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	err := srv.Shutdown(sctx)
	if err != nil {
		_ = srv.Close()
		err = fmt.Errorf("drain incomplete after %s, closed remaining connections: %w", drain, err)
	}
	if serr := <-errc; !errors.Is(serr, http.ErrServerClosed) {
		return serr
	}
	return err
}

// TLSConfig is the one place TLS policy lives. CurvePreferences is left nil on
// purpose: Go's default list prefers the post-quantum hybrid X25519MLKEM768
// (Go 1.24+), so clients that support it already get it.
func TLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		// TLS 1.2 only (1.3 suites are fixed by Go): forward-secret AEAD suites.
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
	}, nil
}
