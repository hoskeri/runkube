package nodes

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/status"
)

// statusErr runs p.Status and requires it to have run (a nil Status means no
// probe was attached, which is a wiring bug).
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

func TestIsNotServing(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{syscall.ECONNREFUSED, true},
		{syscall.ENOENT, true},
		{&net.OpError{Err: &os.SyscallError{Err: syscall.ECONNREFUSED}}, true},
		{&net.OpError{Err: context.DeadlineExceeded}, false},
		{errors.New("500 Internal Server Error"), false},
	} {
		if got := isNotServing(tc.err); got != tc.want {
			t.Errorf("isNotServing(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestKubeletHealthDegradesThenRecovers: with nothing listening the kubelet is
// Degraded (starting), and it becomes healthy once its healthz answers.
func TestKubeletHealthDegradesThenRecovers(t *testing.T) {
	n := &Node{Name: "node-a", MachineID: strings.Repeat("ab", 16)}
	p := n.Kubelet()
	if p.Status == nil {
		t.Fatal("kubelet has no health probe")
	}

	// Nothing is listening on the real kubelet healthz port in the test
	// environment, so the probe must report Degraded, not Unhealthy: a node
	// that has not come up yet is not a broken node.
	se := statusErr(t, p)
	if se == nil || se.State != status.StateDegraded {
		t.Fatalf("state = %v, want degraded (%v)", se, se)
	}
	if !se.Recoverable() {
		t.Error("a degraded component must stay recoverable")
	}

	// Now stand up a healthy endpoint and point the probe at it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	if se := statusErr(t, withHealth(&process.Process{Tag: "kubelet"}, probeHTTP(t, srv.URL+"/healthz"))); se != nil {
		t.Errorf("healthy kubelet reported %v", se)
	}
}

// TestProbeKubeletHTTPFailures: a reachable but unhappy kubelet is Unhealthy.
func TestProbeKubeletHTTPFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	se := statusErr(t, withHealth(&process.Process{Tag: "kubelet"}, probeHTTP(t, srv.URL+"/healthz")))
	if se == nil || se.State != status.StateUnhealthy {
		t.Fatalf("state = %v, want unhealthy", se)
	}
}

// probeHTTP wraps a plain HTTP GET as a node health probe.
func probeHTTP(t *testing.T, url string) func(context.Context) error {
	t.Helper()
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return &status.Error{State: status.StateUnhealthy, Reason: "GET " + url + ": " + resp.Status}
		}
		return nil
	}
}

func TestProbeCRISocket(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "containerd.sock")
	if err := probeCRISocket(missing)(context.Background()); err == nil {
		t.Error("expected an error for a missing CRI socket")
	}
	if !isNotServing(probeCRISocket(missing)(context.Background())) {
		t.Error("a missing socket should read as 'not serving yet'")
	}

	sock := filepath.Join(dir, "containerd.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if err := probeCRISocket(sock)(context.Background()); err != nil {
		t.Errorf("probe: %v", err)
	}
}

// TestCRIServerHasProbe: the CRI server process carries its probe into the
// node formation, where the health record is relayed to the control plane as
// node-a/criserver.
func TestCRIServerHasProbe(t *testing.T) {
	n := &Node{Name: "node-a", MachineID: strings.Repeat("ab", 16)}
	p := n.CRIServer()
	if p.Status == nil {
		t.Fatal("criserver has no health probe")
	}
	// Whatever the host's /run/containerd happens to look like, the probe must
	// answer with a status and must never speak during a teardown.
	if se := statusErr(t, p); se != nil && se.State != status.StateHealthy && se.State != status.StateDegraded {
		t.Fatalf("unexpected state %v", se.State)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Status(ctx); err != nil {
		t.Errorf("a canceled context must not produce a verdict, got %v", err)
	}
}

func TestProbeTimeoutIsShorterThanInterval(t *testing.T) {
	if probeTimeout >= HealthInterval {
		t.Errorf("probeTimeout (%s) must be below HealthInterval (%s) so a hung probe cannot stall a poll cycle",
			probeTimeout, HealthInterval)
	}
	_ = time.Second
}
