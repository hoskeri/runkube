package frontend

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hoskeri/runkube/pkg/server"
	"golang.org/x/sync/errgroup"
)

// Frontend is an interface for the frontend server.
type Frontend interface {
	Run(context.Context, string) error
	Register(...BackendServer) error
}

type frontend struct {
	mu       sync.Mutex
	backends map[key]*backend
	logger   *slog.Logger
}

// New creates a new frontend server.  logger is the frontend's own component
// logger (used for proxy and registration messages); nil falls back to
// slog.Default().
func New(logger *slog.Logger) Frontend {
	if logger == nil {
		logger = slog.Default()
	}
	return &frontend{
		backends: map[key]*backend{},
		logger:   logger,
	}
}

// BackendServer represents a backend server to be proxied.
type BackendServer struct {
	ServerName *url.URL
	Handler    http.Handler
	ServerCert func(*tls.ClientHelloInfo) (*tls.Certificate, error)
	// Logger is the component logger applied to this backend's requests and
	// error paths, so each proxied component logs under its own tag.  nil
	// falls back to the frontend's logger.
	Logger *slog.Logger
}

func (f *frontend) Register(b ...BackendServer) error {
	for _, o := range b {
		if err := f.registerOne(o); err != nil {
			return err
		}
	}
	return nil
}

func (f *frontend) registerOne(o BackendServer) error {
	bk, err := backendKey(o.ServerName)
	if err != nil {
		return err
	}

	if _, err := f.backendFor(bk); err == nil {
		return fmt.Errorf("server exists for %q", bk)
	}

	b := &backend{
		cs: make(chan net.Conn, 5),
		k:  bk,
		o:  o,
	}

	f.mu.Lock()
	f.backends[bk] = b
	f.mu.Unlock()

	logger := o.Logger
	if logger == nil {
		logger = f.logger
	}
	logger.Info("register", "k", bk, "backend", o.ServerName)
	return nil
}

func (f *frontend) serve(b *backend) error {
	logger := b.o.Logger
	if logger == nil {
		logger = f.logger
	}

	h := server.WithLogger(logger, b.o.Handler)
	h = server.WithMatchingHost(logger, h, b.o.ServerName.Hostname())

	s := &http.Server{
		Addr: b.k.String(),
		TLSConfig: &tls.Config{
			ClientAuth:     tls.RequestClientCert,
			GetCertificate: b.o.ServerCert,
		},
		Handler: server.WithLogging(logger, h),
	}

	return s.ServeTLS(b, "", "")
}

func (f *frontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT requests allowed", http.StatusMethodNotAllowed)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	a, err := backendKey(r.URL)
	if err != nil {
		f.logger.Error("error building backend key", "err", err, "req", r.URL)
		http.Error(w, fmt.Errorf("bad proxy host: %w", err).Error(), http.StatusBadRequest)
		return
	}

	b, err := f.backendFor(a)
	if err != nil {
		f.logger.Error("error getting backend", "err", err, "k", a)
		http.Error(w, fmt.Errorf("proxy host not found: %w", err).Error(), http.StatusBadGateway)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		f.logger.Error("error hijacking", "err", err, "k", a)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := b.AddConn(clientConn); err != nil {
		f.logger.Error("backend.AddConn error", "b", b.k, "err", err)
		if err := clientConn.Close(); err != nil {
			f.logger.Error("clientConn.Close", "conn", clientConn.LocalAddr().String())
			return
		}
	}

	clientConn.Write([]byte("HTTP/1.1 200 Connected\r\n\r\n"))
}

func (f *frontend) Run(ctx context.Context, sock string) error {
	eg, ctx := errgroup.WithContext(ctx)
	for _, v := range f.backends {
		eg.Go(func() error {
			return f.serve(v)
		})
	}

	eg.Go(func() error {
		fs := &http.Server{Handler: f}
		ul, err := net.Listen("unix", sock)
		if err != nil {
			return err
		}
		return fs.Serve(ul)
	})

	return eg.Wait()
}

func (f *frontend) backendFor(k key) (*backend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.backends[k]
	if !ok {
		return nil, fmt.Errorf("no proxy registered for %q", k.String())
	}
	return b, nil
}

type backend struct {
	k key
	o BackendServer

	// dispatch to listener
	cs     chan net.Conn
	closed atomic.Bool
}

var _ net.Listener = &backend{}

func (b *backend) AddConn(c net.Conn) error {
	select {
	case b.cs <- c:
		return nil
	case <-time.After(1 * time.Second):
		return fmt.Errorf("timed out adding connection")
	default:
		return fmt.Errorf("error adding conn")
	}
}

func (b *backend) Accept() (net.Conn, error) {
	select {
	case c := <-b.cs:
		return c, nil
	}
}

func (b *backend) Addr() net.Addr {
	return b.k
}

func (b *backend) Close() error {
	if b.closed.Swap(true) {
		return fmt.Errorf("already closed")
	}
	return nil
}

type key struct {
	k string
}

func (k key) Network() string {
	return "proxyhost"
}

func (k key) String() string {
	return k.k
}

func backendKey(u *url.URL) (key, error) {
	h := u.Hostname()
	p := u.Port()
	if p == "" {
		p = "443"
	}

	if p != "443" {
		return key{}, fmt.Errorf("invalid port %s", p)
	}
	return key{
		k: net.JoinHostPort(h, p),
	}, nil
}
