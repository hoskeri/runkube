package nodes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/termhandler"
	"github.com/hoskeri/runkube/pkg/cri"
	"github.com/hoskeri/runkube/pkg/grpcserver"
	"github.com/hoskeri/runkube/pkg/randid"
	"github.com/hoskeri/runkube/pkg/ref"
	"github.com/hoskeri/runkube/pkg/sandbox"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

type Node struct {
	Name          string `json:"name"`
	MachineID     string `json:"machineID"`
	Root          string `json:"root"`
	Kubeconfig    string `json:"kubeconfig"`
	KubeletBinary string `json:"kubelet"`
	// LogLevel is the log level for this node's processes (kubelet and
	// criserver), resolved by the control plane from its process.LogLevels
	// policy and serialized into the node's environment.
	LogLevel slog.Level `json:"logLevel"`
}

// NodeEnvVar carries the serialized node configuration from the control plane
// (parent) to node processes (child), mirroring how the sandbox configuration
// is passed from parent to child.
const NodeEnvVar = "RUNKUBE_NODE"

// encodeNode serializes a Node for transfer as an environment variable.
func encodeNode(n *Node) ([]byte, error) {
	return json.Marshal(n)
}

// decodeNode parses a Node serialized by encodeNode.
func decodeNode(d []byte) (*Node, error) {
	n := &Node{}
	dec := json.NewDecoder(bytes.NewReader(d))
	dec.DisallowUnknownFields()
	if err := dec.Decode(n); err != nil {
		return nil, err
	}

	return n, nil
}

// NodeFromEnv returns the Node the parent control plane serialized into the
// environment (see Node.EnvEntry), or an error when it is absent or invalid.
func NodeFromEnv() (*Node, error) {
	enc := os.Getenv(NodeEnvVar)
	if enc == "" {
		return nil, fmt.Errorf("nodes.NodeFromEnv: %s not set", NodeEnvVar)
	}

	d, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, err
	}

	return decodeNode(d)
}

// EnvEntry returns the environment entry ("RUNKUBE_NODE=...") carrying this
// node's serialized configuration, for parent-to-child transfer.
func (c *Node) EnvEntry() (string, error) {
	b, err := encodeNode(c)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s=%s", NodeEnvVar, base64.StdEncoding.EncodeToString(b)), nil
}

// Validate verifies that the Node is complete and well-formed: every field
// must be set, machineID must be a 16-byte hex string, and paths must be
// absolute. It exists to catch parent-to-child transfer mistakes automatically
// (e.g. a field never set, like a flag dropped from a command line, or one
// corrupted in transit) instead of surfacing later as confusing runtime
// failures such as files silently written to relative paths.
func (c *Node) Validate() error {
	if c.Name == "" {
		return fmt.Errorf("nodes.Node.Validate: name not set")
	}
	if c.MachineID == "" {
		return fmt.Errorf("nodes.Node.Validate: machineID not set")
	}
	if len(c.MachineID) != 2*16 {
		return fmt.Errorf("nodes.Node.Validate: machineID %q must be 16 bytes (32 hex chars)", c.MachineID)
	}
	if _, err := hex.DecodeString(c.MachineID); err != nil {
		return fmt.Errorf("nodes.Node.Validate: machineID %q is not hex: %w", c.MachineID, err)
	}
	if c.Root == "" {
		return fmt.Errorf("nodes.Node.Validate: root not set")
	}
	if c.Kubeconfig == "" {
		return fmt.Errorf("nodes.Node.Validate: kubeconfig not set")
	}
	if c.KubeletBinary == "" {
		return fmt.Errorf("nodes.Node.Validate: kubelet not set")
	}
	for _, p := range []string{c.Root, c.Kubeconfig, c.KubeletBinary} {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("nodes.Node.Validate: path %q must be absolute", p)
		}
	}

	return nil
}

func (c *Node) selfBinary() string {
	e, err := os.Executable()
	if err != nil {
		slog.Error("os.Executable", "err", err)
		os.Exit(1)
	}

	return e
}

func (c *Node) makeKubeletConfig() error {
	kc := fmt.Sprintf(`
kind: KubeletConfiguration
apiVersion: "kubelet.config.k8s.io/v1beta1"
featureGates:
	AllAlpha: true
	AllBeta: true
	KubeletInUserNamespace: true
logging:
	format: "json"
	verbosity: 0
staticPodPath: ""
readOnlyPort: 0
healthzBindAddress: "127.0.0.1"
volumePluginDir: ""
failSwapOn: false
nodeStatusMaxImages: 0
oomScoreAdj: 200
protectKernelDefaults: false
streamingConnectionIdleTimeout: "1m"
enableProfilingHandler: false
enableDebugFlagsHandler: false
enableDebuggingHandlers: false
enableSystemLogHandler: false
enableSystemLogQuery: false
serverTLSBootstrap: false
rotateCertificates: false
containerLogMaxSize: "-1"
cgroupDriver: cgroupfs
cgroupsPerQOS: false
clusterDNS: [ %q ]
enforceNodeAllocatable: [ "none" ]
authentication:
  anonymous:
    enabled: true
	webhook:
		enabled: true
authorization:
	mode: Webhook
`, c.nodeAddress())
	return c.configRef("kubelet-config").Write([]byte(kc))
}

func (c *Node) Kubelet() *process.Process {
	p := &process.Process{
		Tag: "kubelet",
		CmdArgs: []string{
			"kubelet",
			"--config", c.configRef("kubelet-config").Path,
			"--kubeconfig", "/etc/kubernetes/kubelet.kubeconfig",
			// TODO automatically derive node-ip(s) from sandbox environment
			// Kubelet looks for an interface with a default route
			// which is used to grab addresses. So sandbox.go should set up a default route.
			"--node-ip", c.nodeAddress(),
		},
	}
	return withHealth(p, probeKubeletHealth)
}

func (c *Node) configRef(s string) *ref.Config {
	return &ref.Config{
		Path: filepath.Join(c.Root, "etc/kubernetes", s),
	}
}

func (c *Node) ContainerRuntimeEndpoint() string {
	return "/run/containerd/containerd.sock"
}

// nodeAddress returns the node's emulated IPv4 address, derived
// deterministically from the machine id (see randid.IPv4) so it is predictable
// across restarts without being hardcoded. The sandbox's default interface
// carries the same address.
func (c *Node) nodeAddress() string {
	return randid.IPv4(c.MachineID).String()
}

func (c *Node) CRIServer() *process.Process {
	// The criserver process re-executes this binary (selfBinary) and re-enters
	// nodes.Main, which reconstructs the Node from the environment, so the
	// serialized node config must be passed down like the parent does.
	env, err := c.EnvEntry()
	if err != nil {
		panic(err)
	}

	return withHealth(&process.Process{
		Tag:     "criserver",
		CmdArgs: []string{c.selfBinary(), "criserver"},
		Environ: []string{env},
	}, probeCRISocket(c.ContainerRuntimeEndpoint()))
}

func (c *Node) Init(ctx context.Context) error {
	for _, err := range []error{
		c.makeKubeletConfig(),
	} {
		if err != nil {
			return err
		}
	}

	if err := os.RemoveAll(c.ContainerRuntimeEndpoint()); err != nil {
		return err
	}

	return nil
}

func (c *Node) Run(ctx context.Context) error {
	logs := termhandler.New(ctx, os.Stdin, os.Stdout, os.Stderr, &termhandler.Options{
		Level: c.LogLevel,
	})
	defer logs.Close()

	fm := &process.Formation{
		Name:    "runkube",
		Workdir: c.Root,
		Logs:    logs,
		Processes: []*process.Process{
			c.Kubelet(),
			c.CRIServer(),
		},
		LogLevels: process.LogLevels{Default: c.LogLevel},
		// Probe the kubelet and the CRI server on the node's own cadence; the
		// node's own formation is not torn down on a health verdict, so a
		// sick node is reported to the control plane and left to heal.
		StatusInterval: HealthInterval,
	}

	return fm.Run(ctx)
}

func (c *Node) RunSandbox(ctx context.Context) error {
	sc, err := c.sandboxConfig()
	if err != nil {
		return err
	}
	s, err := sandbox.New(sc)
	if err != nil {
		slog.Error("sandbox.Run", "err", err)
		os.Exit(1)
	}

	return s.Run()
}

func (c *Node) sandboxConfig() (*sandbox.Config, error) {
	// The serialized node configuration rides along in the sandbox
	// environment so the node process inside the sandbox can reconstruct
	// this Node without command line flags.
	env, err := c.EnvEntry()
	if err != nil {
		return nil, err
	}

	return &sandbox.Config{
		Version:   "v1",
		Hostname:  c.Name,
		MachineID: c.MachineID,
		IPAddress: c.nodeAddress(),
		Command:   []string{"/usr/bin/breakbulk", "run"},
		Env: []string{
			"HOME=/root",
			"PATH=/sbin:/usr/sbin:/usr/local/bin:/usr/bin:/bin",
			env,
		},
		Payload: []sandbox.Payload{
			{Target: "/etc/kubernetes/kubelet.kubeconfig", Source: sandbox.PayloadSource{Type: "bind", Value: c.Kubeconfig}},
			{Target: "/usr/bin/breakbulk", Source: sandbox.PayloadSource{Type: "bind", Value: c.selfBinary()}},
			{Target: "/usr/bin/kubelet", Source: sandbox.PayloadSource{Type: "bind", Value: c.KubeletBinary}},
			{Target: "/usr/bin/mount", FileMode: 0755, Source: sandbox.PayloadSource{Type: "write", Value: "#!/usr/bin/breakbulk mount\n"}},
		},
		Features: sandbox.Features{
			SyscallFilter: true,
		},
	}, nil
}

func (c *Node) RunCRIServer(ctx context.Context) error {
	b, err := cri.New()
	if err != nil {
		return err
	}

	endpoint := c.ContainerRuntimeEndpoint()
	if err := os.MkdirAll(path.Dir(endpoint), 0700); err != nil {
		return err
	}

	s := grpcserver.New(grpcserver.Config{
		Name:    "cri",
		Network: "unix",
		Address: endpoint,
		Register: func(r *grpc.Server) error {
			reflection.Register(r)
			return cri.Register(r, b)
		},
	})

	return s.ListenAndServe(ctx)
}
