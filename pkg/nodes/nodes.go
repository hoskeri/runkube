package nodes

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/procman/pkg/termhandler"
	"github.com/hoskeri/runkube/pkg/cri"
	"github.com/hoskeri/runkube/pkg/grpcmiddleware"
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
	Address       string `json:"address"`
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
	kc := `
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
clusterDNS: [ "10.0.0.1" ]
enforceNodeAllocatable: [ "none" ]
authentication:
  anonymous:
    enabled: true
	webhook:
		enabled: true
authorization:
	mode: Webhook
`

	return c.configRef("kubelet-config").Write([]byte(kc))
}

func (c *Node) Kubelet() *process.Process {
	return &process.Process{
		Tag: "kubelet",
		CmdArgs: []string{
			"kubelet",
			"--config", c.configRef("kubelet-config").Path,
			"--kubeconfig", "/etc/kubernetes/kubelet.kubeconfig",
			"--node-ip", c.nodeAddress(),
		},
	}
}

func (c *Node) configRef(s string) *ref.Config {
	return &ref.Config{
		Path: filepath.Join(c.Root, "etc/kubernetes", s),
	}
}

func (c *Node) ContainerRuntimeEndpoint() string {
	return "/run/containerd/containerd.sock"
}

func (c *Node) nodeAddress() string {
	return "10.0.0.1"
}

func (c *Node) CRIServer() *process.Process {
	return &process.Process{
		Tag: "criserver",
		CmdArgs: []string{
			c.selfBinary(),
			"criserver",
		},
	}
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
	fm := &process.Formation{
		Workdir: c.Root,
		Sink: slog.New(termhandler.New(os.Stdout, &termhandler.Options{
			Level: slog.LevelInfo,
		})),
		Processes: []*process.Process{
			c.Kubelet(),
			c.CRIServer(),
		},
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
	return &sandbox.Config{
		Version:   "v1",
		Hostname:  c.Name,
		MachineID: c.MachineID,
		IPAddress: c.nodeAddress(),
		Command:   []string{"/usr/bin/breakbulk", "run"},
		Env: []string{
			"HOME=/root",
			"PATH=/sbin:/usr/sbin:/usr/local/bin:/usr/bin:/bin",
		},
		Payload: []sandbox.Payload{
			{Target: "/etc/kubernetes/kubelet.kubeconfig", Source: sandbox.PayloadSource{Type: "bind", Value: c.Kubeconfig}},

			{Target: "/usr/bin/breakbulk", Source: sandbox.PayloadSource{Type: "bind", Value: c.selfBinary()}},
			{Target: "/usr/bin/kubelet", Source: sandbox.PayloadSource{Type: "bind", Value: c.KubeletBinary}},

			{Target: "/usr/bin/mount", FileMode: 0755, Source: sandbox.PayloadSource{Type: "write", Value: "#!/usr/bin/breakbulk mount\n"}},

			{Target: "/dev/null", Source: sandbox.PayloadSource{Type: "bind", Value: "/dev/null"}},

			{Target: "/sys/devices/system/cpu/online", Source: sandbox.PayloadSource{Type: "write", Value: "0-0"}},
			{Target: "/sys/devices/system/node/node0/cpulist", Source: sandbox.PayloadSource{Type: "write", Value: "0-0"}},
			{Target: "/sys/devices/system/node/node0/meminfo", Source: sandbox.PayloadSource{Type: "write", Value: "Node 0 MemTotal:       1 kB"}},
			{Target: "/sys/devices/system/node/node0/distance", Source: sandbox.PayloadSource{Type: "write", Value: "100"}},
			{Target: "/sys/devices/system/node/node0/cpu0/topology/core_id", Source: sandbox.PayloadSource{Type: "write", Value: "0"}},
			{Target: "/sys/devices/system/node/node0/cpu0/topology/physical_package_id", Source: sandbox.PayloadSource{Type: "write", Value: "0"}},
		},
	}, nil
}

func (c *Node) RunCRIServer(ctx context.Context) error {
	b, err := cri.New()
	if err != nil {
		return err
	}

	s := grpc.NewServer(grpc.ChainUnaryInterceptor(grpcmiddleware.Recover, grpcmiddleware.Logging))
	reflection.Register(s)

	if err := cri.Register(s, b); err != nil {
		return err
	}

	slog.Info("RunServer", "sock", c.ContainerRuntimeEndpoint())
	if err := os.MkdirAll(path.Dir(c.ContainerRuntimeEndpoint()), 0700); err != nil {
		return err
	}

	l, err := net.Listen("unix", c.ContainerRuntimeEndpoint())
	if err != nil {
		return err
	}

	defer l.Close()
	go func() {
		_, cancel := context.WithCancelCause(ctx)
		cancel(s.Serve(l))
	}()

	<-ctx.Done()
	s.Stop()

	return ctx.Err()
}
