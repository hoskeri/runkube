package runkube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/status"
	"github.com/hoskeri/runkube/pkg/client"
	"github.com/hoskeri/runkube/pkg/pki"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcstatus "google.golang.org/grpc/status"
)

// Health checks for the control plane.
//
// Every component gets a Process.Status probe: a cheap, dependency-light
// "is it still serving?" question asked on the procman status interval.  Probes
// never tear a formation down on their own — process liveness is procman's
// job, and the keep-waiting-or-not verdict is FormationStatus below.
//
// The vocabulary is deliberately coarse:
//   - Healthy   — the probe succeeded.
//   - Degraded  — the component is not accepting connections yet, or answered
//     but not well (a 5xx, a partial etcd health).  Retriable.
//   - Unhealthy — the probe failed outright (refused, TLS rejected, timeout).
//     Retriable, but it is what the formation policy escalates on.
//
// A component that is merely slow to start therefore does not scream: it is
// Degraded while its socket is missing and becomes Healthy on its own.

// HealthInterval is how often the control plane's health probes run: it is the
// formation's procman StatusInterval, so every component shares one cadence.
const HealthInterval = 5 * time.Second

// probeTimeout bounds a single probe. It is well below HealthInterval so a
// hung endpoint cannot stall a poll cycle.
const probeTimeout = 3 * time.Second

// apiServerPort is the apiserver's secure port (see APIServer).
const apiServerPort = 6443

// controllerManagerPort and schedulerPort are the secure ports the controller
// manager and the scheduler serve /livez on. They are passed explicitly on the
// command lines (see Controller, Scheduler) so the probe cannot drift from the
// process it is probing.
const (
	controllerManagerPort = 10257
	schedulerPort         = 10259
)

// withHealth attaches a probe to a process description. probe returns nil when
// healthy, and an error describing what went wrong otherwise.
func (c *ControlPlane) withHealth(p *process.Process, probe func(context.Context) error) *process.Process {
	p.Status = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()

		if err := probe(ctx); err != nil {
			// A teardown in progress is not a health verdict.
			if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			// Not accepting connections yet, and a server that is up but not
			// ready yet, are both the normal startup state rather than a
			// failure; report them as Degraded so an operator can tell "coming
			// up" from "broken".
			if isNotServing(err) {
				return status.Degraded(err, "%s is not serving yet: %v", p.Tag, err)
			}
			if isNotReady(err) {
				return status.Degraded(err, "%s is not ready yet: %v", p.Tag, err)
			}
			return status.Unhealthy(err, "%s is unhealthy: %v", p.Tag, err)
		}
		return nil
	}
	return p
}

// notServingError marks an error as "nothing is listening yet". It is needed
// for probes whose transport does not preserve the syscall errno — gRPC reports
// a missing listener as codes.Unavailable — so that isNotServing can still tell
// the startup state from a real failure.
type notServingError struct{ err error }

func (e *notServingError) Error() string { return e.err.Error() }
func (e *notServingError) Unwrap() error { return e.err }

// notReadyError marks an error as "the server is answering, but it is not
// ready yet". See isNotReady for why that is not a failure.
type notReadyError struct{ err error }

func (e *notReadyError) Error() string { return e.err.Error() }
func (e *notReadyError) Unwrap() error { return e.err }

// isNotServing reports whether err means "nothing is listening yet" rather than
// "something is listening and it is wrong". Dial errors wrap the syscall errno,
// so errors.Is alone reaches through net.OpError and friends.
func isNotServing(err error) bool {
	if _, ok := errors.AsType[*notServingError](err); ok {
		return true
	}
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// isNotReady reports whether err means "up, but still coming up".
//
// A readiness endpoint answers 500 while its server finishes starting — the
// apiserver does so before its post-start hooks and controllers have settled,
// the scheduler before it has a connection to place pods with. That is a
// startup state, not a fault, so it is Degraded rather than Unhealthy; a
// *live* endpoint (or a readiness 4xx) still counts as unhealthy, because
// there is no such excuse for a running process.
func isNotReady(err error) bool {
	_, ok := errors.AsType[*notReadyError](err)
	return ok
}

// clientTLS builds a TLS client config trusting ca and presenting clientCert
// (nil for server-only probes). Server names are carried by the dialers below,
// so verification uses the leaf as-is (runkube's certificates are issued for
// the internal hostnames).
func (c *ControlPlane) clientTLS(ca string, clientCert *pki.Leaf) (*tls.Config, error) {
	pem, err := os.ReadFile(c.pkiStore().CACertPath(ca))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates in %s", c.pkiStore().CACertPath(ca))
	}

	// The internal listener addresses are covered by the leaves' IP SANs, but
	// the names differ per component; verify against whatever the probe
	// actually connects to.
	cfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if clientCert != nil {
		cert, err := clientCert.TLSCertificate()
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	return cfg, nil
}

// apiClient returns the client used to call the apiserver's own HTTP
// endpoints (/livez, /readyz, ...): TLS against the apiserver CA, verified as
// the internal listen address, presenting the system-admin client certificate
// so authenticated paths are reachable too. The transport is cached per
// endpoint (see client.TransportCache).
func (c *ControlPlane) apiClient() (*client.HTTP, error) {
	addr := c.internalListenAddr()
	target := netip.AddrPortFrom(addr, apiServerPort).String()
	rt, err := c.transports.Get("api", func() (*http.Transport, error) {
		cfg, err := c.clientTLS(pki.APIServer, c.pkiStore().Leaf("system-admin"))
		if err != nil {
			return nil, err
		}
		return client.TLSTransport(target, addr.String(), cfg), nil
	})
	if err != nil {
		return nil, err
	}
	return client.NewHTTP("https", target, rt), nil
}

// componentClient returns the client used to call a kube component's own
// HTTPS endpoints on its secure port. The component's serving leaf is issued
// for hostname(serving), which is what the server is verified as.
func (c *ControlPlane) componentClient(ca, serving string, port uint16) (*client.HTTP, error) {
	addr := c.internalListenAddr()
	target := netip.AddrPortFrom(addr, port).String()
	rt, err := c.transports.Get("component/"+serving, func() (*http.Transport, error) {
		cfg, err := c.clientTLS(ca, nil)
		if err != nil {
			return nil, err
		}
		return client.TLSTransport(target, c.hostname(serving), cfg), nil
	})
	if err != nil {
		return nil, err
	}
	return client.NewHTTP("https", target, rt), nil
}

// unixClient returns the client that speaks plain HTTP over the unix socket
// named socket.
func (c *ControlPlane) unixClient(socket string) (*client.HTTP, error) {
	rt, err := c.transports.Get("unix/"+socket, func() (*http.Transport, error) {
		return client.UnixTransport(socket), nil
	})
	if err != nil {
		return nil, err
	}
	return client.NewHTTP("http", "unix.invalid", rt), nil
}

// getAPI performs a GET against the apiserver.
func (c *ControlPlane) getAPI(ctx context.Context, urlPath string) (int, error) {
	hc, err := c.apiClient()
	if err != nil {
		return 0, err
	}
	return hc.Get(ctx, urlPath)
}

// getUnix performs a plain-HTTP GET over a unix socket.
func (c *ControlPlane) getUnix(ctx context.Context, socket, urlPath string) (int, error) {
	hc, err := c.unixClient(socket)
	if err != nil {
		return 0, err
	}
	return hc.Get(ctx, urlPath)
}

// readyPath is the readiness endpoint. A 5xx from it means "up, not ready
// yet" (see isNotReady); every other path answers 5xx only when something is
// actually wrong.
const readyPath = "/readyz"

// probeStatus turns a GET result into a probe verdict: nil for 200, and an
// *httpError otherwise. A 5xx from a readiness endpoint is wrapped as
// *notReadyError so it lands as Degraded instead of Unhealthy.
func probeStatus(code int, urlPath string) error {
	if code == http.StatusOK {
		return nil
	}
	var err error = &httpError{status: code, url: urlPath}
	if urlPath == readyPath && code >= 500 {
		return &notReadyError{err}
	}
	return err
}

// --- per-component probes ---------------------------------------------------

// probeEtcd asks etcd for a real health verdict over gRPC: the standard
// grpc.health.v1 service that etcd registers on its client socket, answered
// only once the server is actually able to serve gRPC calls. This replaces
// shelling out to `etcdctl endpoint health`, which forked a process on every
// probe and re-derived the same TLS material from disk each time.
func (c *ControlPlane) probeEtcd(ctx context.Context) error {
	socket := c.unixSocketPath("etcd").Path
	cfg, err := c.clientTLS(pki.Etcd, c.pkiStore().Leaf("apiserver-etcd-client"))
	if err != nil {
		return err
	}
	// etcd's serving leaf carries the socket's base name as its only DNS SAN.
	cfg.ServerName = filepath.Base(socket)

	gc, err := client.Dial("unix://"+socket, credentials.NewTLS(cfg))
	if err != nil {
		return &notServingError{err}
	}
	defer gc.Close()

	err = gc.HealthCheck(ctx, "")
	switch {
	case err == nil:
	case grpcstatus.Code(err) == codes.Unavailable:
		// gRPC erases the syscall errno behind the dial, so "nobody is
		// listening" and "the server hung up" both arrive as Unavailable.
		// The former is the startup state, not a failure.
		return &notServingError{fmt.Errorf("etcd health: %w", err)}
	default:
		return fmt.Errorf("etcd health: %w", err)
	}
	return nil
}

// probeAPIServer checks both liveness and readiness: /livez says the process
// is up, /readyz says it is willing to serve.
func (c *ControlPlane) probeAPIServer(ctx context.Context) error {
	for _, p := range []string{"/livez", readyPath} {
		code, err := c.getAPI(ctx, p)
		if err != nil {
			return err
		}
		if err := probeStatus(code, p); err != nil {
			return err
		}
	}
	return nil
}

// probeComponentStatus asks a kube component for its own status verdict.
//
// The controller manager and the scheduler are ordinary exec'd binaries, but
// they are not opaque: runkube hands them a bind address and a generated
// serving certificate, and both serve health endpoints on their secure port
// without authentication. So their probe talks to the component itself rather
// than inferring liveness from elsewhere. paths lists the endpoints that
// component actually installs — kube-controller-manager serves /healthz only,
// while kube-scheduler serves the full /healthz, /livez, /readyz set.
func (c *ControlPlane) probeComponentStatus(ca, serving string, port uint16, paths ...string) func(context.Context) error {
	return func(ctx context.Context) error {
		hc, err := c.componentClient(ca, serving, port)
		if err != nil {
			return err
		}
		for _, p := range paths {
			code, err := hc.Get(ctx, p)
			if err != nil {
				return err
			}
			if err := probeStatus(code, p); err != nil {
				return err
			}
		}
		return nil
	}
}

// probeWebhookFrontend checks the webhook process's *frontend*: the CONNECT
// proxy in front of every admission/authentication/authorization backend.
//
// The frontend speaks plain HTTP on an abstract unix socket (the TLS backends
// hang behind it, one per registered server name), so the probe is a GET: a
// 405 "only CONNECT requests allowed" is the healthy answer and proves the
// socket is accepting. A transport error means nothing is listening yet.
func (c *ControlPlane) probeWebhookFrontend(ctx context.Context) error {
	// The frontend listens on an *abstract* unix socket, addressed by name
	// rather than by path.
	sock := c.unixSocketURL("frontend").Opaque
	if _, err := c.getUnix(ctx, sock, "/"); err != nil {
		return err
	}
	return nil
}

// httpError carries a non-200 HTTP status as an error.
type httpError struct {
	status int
	url    string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("GET %s: HTTP %d", e.url, e.status)
}

// probeUnixSocketAccepting checks that a unix-socket server is accepting
// connections, i.e. that a listener exists on that socket. Used for the
// in-process gRPC servers (kms, jwtsigner), whose sockets are abstract
// ("@<name>-<tag>") and therefore have no filesystem path to stat.
func probeUnixSocketAccepting(socket string) func(context.Context) error {
	return func(ctx context.Context) error {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", socket)
		if err != nil {
			return err
		}
		return conn.Close()
	}
}

// --- wiring -----------------------------------------------------------------

// Processes returns the control-plane process list. Each component attaches its
// own health probe as it is described (see comps.go), the way the node's
// processes do in pkg/nodes; the one-shot subcommands (kms, jwtsigner) get
// probes too, since they stay up for the life of the formation.
func (c *ControlPlane) healthProcesses() []*process.Process {
	ps := []*process.Process{
		c.Etcd(),
		c.APIServer(),
		c.Controller(),
		c.Scheduler(),
		c.Webhook(),
		c.KMSPlugin(),
		c.JWTSigner(),
	}
	return append(ps, c.nodeProcesses()...)
}

// --- formation-level policy -------------------------------------------------
// health tracks the formation verdict between polls.
type health struct {
	mu sync.Mutex
	// unhealthySince is when a critical component first went bad; the grace
	// period runs from then.
	unhealthySince time.Time
	// verdict is the last logged summary, so it is logged only on change.
	verdict string
	// grace is how long a critical component may be bad before the formation
	// gives up.
	grace time.Duration
}

// healthSummary is the compact, logfmt-friendly rendering of a status view.
func healthSummary(v process.StatusView) string {
	s := make([]string, 0, len(v.Components))
	for _, comp := range v.Components {
		state := comp.State.String()
		if comp.Exited {
			state += "/exited"
		}
		s = append(s, fmt.Sprintf("%s=%s", comp.Identity(), state))
	}
	return strings.Join(s, " ")
}

// critical reports whether a component's state should escalate the formation.
//
// The two rules, in order:
//
//   - A component that reported Failed is always critical. That state is an
//     explicit "I am done" (a child pushing status through its reporter, not a
//     probe), and honoring it is the whole point of the vocabulary.
//   - Otherwise only etcd and the apiserver escalate on their own: the control
//     plane cannot function without its store or its API, whereas a flapping
//     webhook, scheduler or node is a self-healing wobble we report and wait
//     out.
//
// A process that exited, or that vanished without reporting, is never critical
// here: procman's exit semantics already bring the formation down, so the
// health policy must not double-report it.
func critical(identity string, c process.ComponentStatus) bool {
	if c.Exited || c.State == status.StateUnknownProcess {
		return false
	}
	if c.State == status.StateFailed {
		return true
	}
	switch identity {
	case "etcd", "api":
		return c.State == status.StateUnhealthy
	}
	return false
}

// FormationStatus is the control plane's health policy: the one place allowed
// to bring the formation down for a health reason.
//
// It reads the aggregate view procman attaches to ctx. A critical component
// (etcd, the apiserver) that stays unhealthy for longer than HealthGrace is
// treated as fatal — a control plane without its store or its API is not going
// to recover by itself. Everything else (a flapping webhook, a node that has
// not come up yet) is reported and waited out.
func (c *ControlPlane) FormationStatus() func(context.Context) error {
	return c.formationStatusWith(&health{grace: c.HealthGrace})
}

// formationStatusWith is FormationStatus with an injectable health tracker
// (tests drive the clock through it).
func (c *ControlPlane) formationStatusWith(h *health) func(context.Context) error {
	return func(ctx context.Context) error {
		v, ok := process.StatusViewFromContext(ctx)
		if !ok {
			return nil
		}

		var bad []string
		for _, comp := range v.Components {
			if critical(comp.Identity(), comp) {
				bad = append(bad, fmt.Sprintf("%s(%s)", comp.Identity(), comp.State))
			}
		}

		h.mu.Lock()
		defer h.mu.Unlock()

		switch {
		case len(bad) == 0:
			h.unhealthySince = time.Time{}
		case h.unhealthySince.IsZero():
			h.unhealthySince = time.Now()
			slog.Warn("control plane degraded",
				"components", strings.Join(bad, " "),
				"grace", h.grace.String(),
				"detail", healthSummary(v))
		default:
			if time.Since(h.unhealthySince) < h.grace {
				// Still inside the grace window: report, do not escalate.
				slog.Debug("control plane still degraded",
					"components", strings.Join(bad, " "),
					"for", time.Since(h.unhealthySince).Truncate(time.Second).String(),
					"grace", h.grace.String())
				return nil
			}
			slog.Error("control plane unhealthy, tearing down",
				"components", strings.Join(bad, " "),
				"for", time.Since(h.unhealthySince).Truncate(time.Second).String())
			return status.Failed(nil, "critical components unhealthy: %s", strings.Join(bad, " "))
		}

		if s := healthSummary(v); s != h.verdict {
			h.verdict = s
			slog.Info("control plane status", "components", s)
		}
		return nil
	}
}
