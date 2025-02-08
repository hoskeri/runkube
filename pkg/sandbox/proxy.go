package sandbox

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type AddressFilter func(context.Context, *url.URL) (string, error)

func PassThrough() AddressFilter {
	return func(_ context.Context, u *url.URL) (string, error) {
		h := u.Hostname()
		p := u.Port()
		if p == "" {
			p = "443"
		}
		return net.JoinHostPort(h, p), nil
	}
}

func SingleHost(addrPort string) AddressFilter {
	return func(_ context.Context, _ *url.URL) (string, error) {
		return addrPort, nil
	}
}

var defaultDialer = &net.Dialer{
	Timeout: 5 * time.Second,
}

type Forwarder struct {
	dialer *net.Dialer
	filter AddressFilter
}

func NewForwarder(f AddressFilter) (*Forwarder, error) {
	return &Forwarder{
		dialer: defaultDialer,
		filter: f,
	}, nil
}

func (e Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != "CONNECT" {
		http.Error(w, "", http.StatusMethodNotAllowed)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	da, err := e.filter(ctx, r.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	destConn, err := e.dialer.DialContext(r.Context(), "tcp", da)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusOK)
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	go transfer(destConn, clientConn)
	transfer(clientConn, destConn)
}

func transfer(destination io.WriteCloser, source io.ReadCloser) {
	defer destination.Close()
	defer source.Close()
	io.Copy(destination, source)
}
