package runkube

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/status"
)

// view builds the aggregate procman hands to the formation's Status callback.
func view(components ...process.ComponentStatus) context.Context {
	return process.WithStatusView(context.Background(), process.StatusView{Components: components})
}

func comp(tag string, st status.State) process.ComponentStatus {
	return process.ComponentStatus{Tag: tag, State: st}
}

func TestCritical(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    process.ComponentStatus
		want bool
	}{
		{"etcd unhealthy", comp("etcd", status.StateUnhealthy), true},
		{"etcd degraded is not critical", comp("etcd", status.StateDegraded), false},
		{"etcd healthy", comp("etcd", status.StateHealthy), false},
		{"api unhealthy", comp("api", status.StateUnhealthy), true},
		{"webhook unhealthy is not critical", comp("webhook", status.StateUnhealthy), false},
		{"webhook failed is always critical", comp("webhook", status.StateFailed), true},
		{"node failed is always critical", comp("node-a", status.StateFailed), true},
		{"node unhealthy is not critical", comp("node-a", status.StateUnhealthy), false},
		// A critical process that exited already brings the formation down
		// through procman's exit semantics; the health policy must not
		// double-report it.
		{"exited etcd", process.ComponentStatus{Tag: "etcd", State: status.StateHealthy, Exited: true}, false},
		{"gone etcd", comp("etcd", status.StateUnknownProcess), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := critical(tc.c.Identity(), tc.c); got != tc.want {
				t.Errorf("critical = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHealthSummary(t *testing.T) {
	s := healthSummary(process.StatusView{Components: []process.ComponentStatus{
		comp("etcd", status.StateHealthy),
		comp("api", status.StateUnhealthy),
		{Tag: "api", Index: 2, State: status.StateHealthy, Exited: true},
	}})
	for _, want := range []string{"etcd=healthy", "api=unhealthy", "api-2=healthy/exited"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q missing %q", s, want)
		}
	}
}

// TestFormationStatusGrace pins the policy the control plane actually cares
// about: an unhealthy apiserver is reported and waited out, and only after the
// grace period does it bring the cluster down.
func TestFormationStatusGrace(t *testing.T) {
	c := &ControlPlane{}
	h := &health{grace: time.Minute}
	policy := c.formationStatusWith(h)

	ctx := view(comp("etcd", status.StateHealthy), comp("api", status.StateUnhealthy))

	if err := policy(ctx); err != nil {
		t.Fatalf("first unhealthy poll must be tolerated, got %v", err)
	}
	if h.unhealthySince.IsZero() {
		t.Error("the grace clock did not start")
	}

	// Still inside the grace window.
	h.unhealthySince = time.Now().Add(-30 * time.Second)
	if err := policy(ctx); err != nil {
		t.Fatalf("inside the grace window: got %v", err)
	}

	// Past the grace window: fatal.
	h.unhealthySince = time.Now().Add(-2 * time.Minute)
	err := policy(ctx)
	if err == nil {
		t.Fatal("expected a fatal status past the grace window")
	}
	var se *status.Error
	if !errors.As(err, &se) {
		t.Fatalf("got %T, want *status.Error", err)
	}
	if !se.Fatal() || se.Recoverable() {
		t.Errorf("expected a non-recoverable status, got %+v", se)
	}
	if !strings.Contains(se.Reason, "api") {
		t.Errorf("reason should name the component: %q", se.Reason)
	}
}

// TestFormationStatusRecovers: the grace clock is cleared when the cluster
// comes back, so a later, unrelated blip gets the full window again.
func TestFormationStatusRecovers(t *testing.T) {
	c := &ControlPlane{}
	h := &health{grace: time.Minute}
	policy := c.formationStatusWith(h)

	bad := view(comp("api", status.StateUnhealthy))
	good := view(comp("api", status.StateHealthy))

	if err := policy(bad); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if err := policy(good); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !h.unhealthySince.IsZero() {
		t.Error("recovery must reset the grace clock")
	}
	if err := policy(bad); err != nil {
		t.Fatalf("a new blip must be tolerated again, got %v", err)
	}
}

// TestFormationStatusNonCritical: an unhealthy (but not failed) webhook is
// reported and waited out, even long past the grace window.
func TestFormationStatusNonCritical(t *testing.T) {
	c := &ControlPlane{}
	h := &health{grace: time.Nanosecond}
	policy := c.formationStatusWith(h)

	ctx := view(comp("webhook", status.StateUnhealthy), comp("node-a", status.StateUnhealthy))
	h.unhealthySince = time.Now().Add(-time.Hour)
	if err := policy(ctx); err != nil {
		t.Fatalf("only etcd, the apiserver, and explicit failures are critical; got %v", err)
	}
	if !h.unhealthySince.IsZero() {
		t.Error("a non-critical problem must not start the grace clock")
	}
}

// TestFormationStatusFailedIsFatal: a component that reported Failed brings
// the formation down once the grace window has passed, whatever it is.
func TestFormationStatusFailedIsFatal(t *testing.T) {
	c := &ControlPlane{}
	h := &health{grace: time.Minute}
	policy := c.formationStatusWith(h)

	ctx := view(comp("webhook", status.StateFailed))
	if err := policy(ctx); err != nil {
		t.Fatalf("inside the grace window: %v", err)
	}
	h.unhealthySince = time.Now().Add(-2 * time.Minute)
	if err := policy(ctx); err == nil {
		t.Fatal("a failed component past the grace window must be fatal")
	}
}

func TestFormationStatusWithoutView(t *testing.T) {
	c := &ControlPlane{}
	if err := c.formationStatusWith(&health{grace: time.Minute})(context.Background()); err != nil {
		t.Errorf("no view attached should be a no-op, got %v", err)
	}
}

func TestIsNotServing(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{syscall.ECONNREFUSED, true},
		{syscall.ENOENT, true},
		{&net.OpError{Err: &os.SyscallError{Err: syscall.ECONNREFUSED}}, true},
		{syscall.ETIMEDOUT, false},
		{errors.New("500 Internal Server Error"), false},
		{nil, false},
	} {
		if got := isNotServing(tc.err); got != tc.want {
			t.Errorf("isNotServing(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestProbeAbstractSocket: the gRPC servers listen on abstract unix sockets
// (no filesystem path), which is what kms and jwtsigner are probed on.
func TestProbeAbstractSocket(t *testing.T) {
	name := fmt.Sprintf("@runkube-test-%d.sock", time.Now().UnixNano())
	ln, err := net.Listen("unix", name)
	if err != nil {
		t.Skipf("abstract unix sockets unavailable: %v", err)
	}
	defer ln.Close()

	if err := probeUnixSocketAccepting(name)(context.Background()); err != nil {
		t.Errorf("probe: %v", err)
	}
	if err := probeUnixSocketAccepting(name + "-nope")(context.Background()); err == nil {
		t.Error("expected an error for an unlistened socket name")
	} else if !isNotServing(err) {
		t.Errorf("an unlistened socket should read as 'not serving yet': %v", err)
	}
}

// TestGetOverUnixSocket covers the probe transport the webhook check relies on:
// a request that crosses a unix socket, with any status treated as
// "listening".
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

	code, err := get(context.Background(), unixTransportFor(sock), "http", "webhook.invalid", "/")
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

	client, _ := clientFor(t, cert)
	tr := unixTransportFor(sock)
	tr.TLSClientConfig = client
	if _, err := get(context.Background(), tr, "https", "x.invalid", "/"); err != nil {
		t.Errorf("a trusted server should answer: %v", err)
	}

	empty, err := x509Pool()
	if err != nil {
		t.Fatal(err)
	}
	tr = unixTransportFor(sock)
	tr.TLSClientConfig = &tls.Config{RootCAs: empty, MinVersion: tls.VersionTLS12}
	if _, err := get(context.Background(), tr, "https", "x.invalid", "/"); err == nil {
		t.Error("expected a verification failure against an empty root pool")
	}
}

// unixTransportFor builds a plain transport dialing sock, for the get tests.
func unixTransportFor(sock string) *http.Transport {
	var d net.Dialer
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", sock)
		},
	}
}

// TestTransportCacheMemoizes: the probes share transports per endpoint, so a
// cached one must be reused (and a failed build must not be cached as nil).
func TestTransportCacheMemoizes(t *testing.T) {
	var tc transportCache
	first, err := tc.get("k", func() (*http.Transport, error) { return &http.Transport{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	second, err := tc.get("k", func() (*http.Transport, error) {
		t.Error("cached transport was rebuilt")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("cache returned a different transport for the same key")
	}

	if _, err := tc.get("bad", func() (*http.Transport, error) {
		return nil, errors.New("boom")
	}); err == nil {
		t.Error("expected the build error")
	}
	if _, err := tc.get("bad", func() (*http.Transport, error) { return &http.Transport{}, nil }); err != nil {
		t.Errorf("a failed build must not poison the cache: %v", err)
	}
}

// TestProbeStatus: only a 5xx from the readiness endpoint means "up, coming
// up". Everything else non-200 is a real fault.
func TestProbeStatus(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      int
		path      string
		wantState status.State
	}{
		{"livez ok", http.StatusOK, "/livez", status.StateHealthy},
		{"readyz ok", http.StatusOK, readyPath, status.StateHealthy},
		{"readyz 500 while starting", http.StatusInternalServerError, readyPath, status.StateDegraded},
		{"readyz 503 while starting", http.StatusServiceUnavailable, readyPath, status.StateDegraded},
		{"livez 500 is a fault", http.StatusInternalServerError, "/livez", status.StateUnhealthy},
		{"readyz 401 is a fault", http.StatusUnauthorized, readyPath, status.StateUnhealthy},
		{"healthz 500 is a fault", http.StatusInternalServerError, "/healthz", status.StateUnhealthy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testControlPlane(t).withHealth(&process.Process{Tag: "api"}, func(context.Context) error {
				return probeStatus(tc.code, tc.path)
			})
			se := statusErr(t, p)
			if tc.wantState == status.StateHealthy {
				if se != nil {
					t.Fatalf("status = %v, want healthy", se)
				}
				return
			}
			if se == nil {
				t.Fatalf("status = healthy, want %v", tc.wantState)
			}
			if se.State != tc.wantState {
				t.Errorf("state = %v, want %v (%v)", se.State, tc.wantState, se)
			}
			if tc.wantState == status.StateDegraded && critical("api", process.ComponentStatus{Tag: "api", State: se.State}) {
				t.Error("a degraded component must not escalate the formation")
			}
		})
	}
}

// statusErr runs p.Status and returns the reported status error, or nil when
// the probe reported healthy.
func statusErr(t *testing.T, p *process.Process) *status.Error {
	t.Helper()
	if p.Status == nil {
		t.Fatalf("process %q has no health probe", p.Tag)
	}
	err := p.Status(context.Background())
	if err == nil {
		return nil
	}
	var se *status.Error
	if !errors.As(err, &se) {
		t.Fatalf("probe returned %T (%v), want *status.Error", err, err)
	}
	return se
}

// TestIsNotServingWraps: probes whose transport hides the syscall errno (gRPC
// reports Unavailable) mark the startup state explicitly, and isNotServing must
// see through the wrapper.
func TestIsNotServingWraps(t *testing.T) {
	err := &notServingError{fmt.Errorf("etcd health: %w", syscall.ENOENT)}
	if !isNotServing(err) {
		t.Errorf("notServingError should read as 'not serving yet': %v", err)
	}
	if !isNotServing(fmt.Errorf("probe: %w", err)) {
		t.Error("notServingError must survive wrapping")
	}
}

func TestProbeUnixSocketAccepting(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.sock")
	if err := probeUnixSocketAccepting(missing)(context.Background()); err == nil {
		t.Error("expected an error for a missing socket")
	}

	sock := filepath.Join(dir, "cri.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if err := probeUnixSocketAccepting(sock)(context.Background()); err != nil {
		t.Errorf("probe: %v", err)
	}
}

// selfSigned generates a throwaway server certificate for the probe tests.
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
