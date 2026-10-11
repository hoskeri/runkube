// Package client provides generic HTTP and gRPC clients for the endpoints
// runkube talks to.
//
// The clients know how to move bytes to an endpoint — a TCP address, a unix
// socket, an abstract gRPC socket — and nothing about runkube's own types or
// its health vocabulary. Choosing what to ask, which certificates to present,
// and how to interpret the answer stays with the caller (health.go, comps.go,
// pkg/nodes).
package client

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// idleConnTimeout is how long an idle pooled connection is kept before it is
// closed. The probes run against the same handful of endpoints every poll, so
// a warm connection avoids a fresh handshake each time.
const idleConnTimeout = 30 * time.Second

// HTTP is a client bound to a single HTTP endpoint.
//
// host is used only to build request URLs (and scheme selects TLS or not);
// the transport decides where the bytes actually go, so a host like
// "unix.invalid" works for a unix-socket transport.
type HTTP struct {
	client *http.Client
	scheme string
	host   string
}

// NewHTTP returns a client that sends requests to scheme://host through rt.
func NewHTTP(scheme, host string, rt http.RoundTripper) *HTTP {
	return &HTTP{
		client: &http.Client{Transport: rt},
		scheme: scheme,
		host:   host,
	}
}

// Get performs a GET of urlPath and returns the response status. The body is
// drained and closed so the connection can be pooled and reused.
func (c *HTTP) Get(ctx context.Context, urlPath string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.scheme+"://"+c.host+urlPath, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TLSTransport returns an HTTP transport that dials addr over TCP with TLS,
// verifying the server as serverName. cfg may be nil, in which case the
// default TLS configuration is used.
//
// The internal listener addresses are covered by the leaves' IP SANs, but the
// names differ per component; callers pass the name the probe actually
// connects to.
func TLSTransport(addr, serverName string, cfg *tls.Config) *http.Transport {
	if cfg == nil {
		cfg = &tls.Config{}
	}
	cfg.ServerName = serverName
	return &http.Transport{
		TLSClientConfig: cfg,
		IdleConnTimeout: idleConnTimeout,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
	}
}

// UnixTransport returns an HTTP transport that dials socket — a filesystem
// path or an abstract "@name" — and speaks plain HTTP. The host in the request
// URL is unused (the dialer ignores it) but must be syntactically valid.
func UnixTransport(socket string) *http.Transport {
	return &http.Transport{
		IdleConnTimeout: idleConnTimeout,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
}

// TransportCache memoizes HTTP transports by key.
//
// Callers build one transport per endpoint and reuse it, so each request
// reuses the established connection and the already-parsed TLS material
// instead of re-reading it from disk and re-handshaking every poll. A
// transport is safe for concurrent use.
//
// The zero value is ready to use.
type TransportCache struct {
	mu sync.Mutex
	m  map[string]*http.Transport
}

// Get returns the transport cached under key, building and caching it on the
// first call. A failed build is not cached, so a later call can retry.
func (tc *TransportCache) Get(key string, build func() (*http.Transport, error)) (*http.Transport, error) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.m == nil {
		tc.m = make(map[string]*http.Transport)
	}
	if tr, ok := tc.m[key]; ok {
		return tr, nil
	}
	tr, err := build()
	if err != nil {
		return nil, err
	}
	tc.m[key] = tr
	return tr, nil
}
