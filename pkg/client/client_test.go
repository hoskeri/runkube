package client

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// TestGetOverUnixSocket: a GET crosses a unix socket and returns the server's
// status, with the URL host ignored by the dialer.
func TestGetOverUnixSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "frontend.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}),
	}
	go srv.Serve(ln)

	hc := NewHTTP("http", "webhook.invalid", UnixTransport(sock))
	code, err := hc.Get(context.Background(), "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

// TestGetTLSVerification: a probe that does not verify must fail, otherwise a
// health check could be fooled by anything listening on the socket.
func TestGetTLSVerification(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "frontend.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cert := selfSigned(t)
	srv := &http.Server{
		Handler:   http.NotFoundHandler(),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	go srv.ServeTLS(ln, "", "")

	trusted, _ := clientFor(t, cert)
	tr := UnixTransport(sock)
	tr.TLSClientConfig = trusted
	hc := NewHTTP("https", "x.invalid", tr)
	if _, err := hc.Get(context.Background(), "/"); err != nil {
		t.Errorf("a trusted server should answer: %v", err)
	}

	empty, err := x509Pool()
	if err != nil {
		t.Fatal(err)
	}
	tr = UnixTransport(sock)
	tr.TLSClientConfig = &tls.Config{RootCAs: empty, MinVersion: tls.VersionTLS12}
	hc = NewHTTP("https", "x.invalid", tr)
	if _, err := hc.Get(context.Background(), "/"); err == nil {
		t.Error("expected a verification failure against an empty root pool")
	}
}

// TestTransportCacheMemoizes: the probes share transports per endpoint, so a
// cached one must be reused (and a failed build must not be cached as nil).
func TestTransportCacheMemoizes(t *testing.T) {
	var tc TransportCache
	first, err := tc.Get("k", func() (*http.Transport, error) { return &http.Transport{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	second, err := tc.Get("k", func() (*http.Transport, error) {
		t.Error("cached transport was rebuilt")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("cache returned a different transport for the same key")
	}

	if _, err := tc.Get("bad", func() (*http.Transport, error) {
		return nil, errors.New("boom")
	}); err == nil {
		t.Error("expected the build error")
	}
	if _, err := tc.Get("bad", func() (*http.Transport, error) { return &http.Transport{}, nil }); err != nil {
		t.Errorf("a failed build must not poison the cache: %v", err)
	}
}

// TestGRPCHealthCheck: the generic gRPC client reaches a health service over a
// unix socket and reports SERVING.
func TestGRPCHealthCheck(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "health.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go srv.Serve(ln)
	defer srv.Stop()

	conn, err := Dial("unix://"+sock, insecure.NewCredentials())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := conn.HealthCheck(context.Background(), "")
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became healthy: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// selfSigned generates a throwaway server certificate for the client tests.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"x.invalid", "webhook.invalid"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// clientFor returns a TLS client config trusting cert.
func clientFor(t *testing.T, cert tls.Certificate) (*tls.Config, *x509.CertPool) {
	t.Helper()
	pool := x509.NewCertPool()
	for _, der := range cert.Certificate {
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		pool.AddCert(leaf)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, pool
}

// x509Pool returns an empty (trust-nothing) pool.
func x509Pool() (*x509.CertPool, error) {
	return x509.NewCertPool(), nil
}
