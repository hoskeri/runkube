package nodes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/status"
)

// Health checks for the processes inside a node sandbox.
//
// The node's kubelet binds its healthz server to 127.0.0.1 (see
// makeKubeletConfig), so it is only reachable from inside the sandbox — these
// probes are necessarily node-local, which is exactly why they live with the
// node rather than with the control plane that owns the process.

// HealthInterval is how often the node's probes run.
const HealthInterval = 5 * time.Second

// probeTimeout bounds a single probe.
const probeTimeout = 3 * time.Second

// KubeletHealth is the kubelet's healthz endpoint inside the sandbox.
const KubeletHealth = "http://127.0.0.1:10248/healthz"

// withHealth attaches a probe to a process. A process that is not accepting
// connections yet is Degraded (the normal state during startup); anything else
// is Unhealthy. Neither ends the formation on its own: a node losing its
// kubelet should be visible, not fatal to the cluster.
func withHealth(p *process.Process, probe func(context.Context) error) *process.Process {
	p.Status = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()

		err := probe(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() == context.Canceled {
			// The formation is coming down; this is not a verdict.
			return nil
		}
		if isNotServing(err) {
			return status.Degraded(err, "%s is not serving yet: %v", p.Tag, err)
		}
		return status.Unhealthy(err, "%s is unhealthy: %v", p.Tag, err)
	}
	return p
}

func isNotServing(err error) bool {
	if os.IsNotExist(err) {
		return true
	}
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// probeKubeletHealth asks the kubelet for its own health verdict.
func probeKubeletHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, KubeletHealth, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", KubeletHealth, resp.Status)
	}
	return nil
}

// probeCRISocket checks that the CRI gRPC server is accepting connections on
// its unix socket. The socket's presence plus a successful dial is the whole
// check; the gRPC handshake is not needed to tell "up" from "down".
func probeCRISocket(path string) func(context.Context) error {
	return func(ctx context.Context) error {
		if _, err := os.Stat(path); err != nil {
			return err
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", path)
		if err != nil {
			return err
		}
		return conn.Close()
	}
}
