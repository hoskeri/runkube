package nodes

import (
	"context"
	"os"

	"github.com/spf13/pflag"
	"log/slog"
)

func Main(ctx context.Context, args ...string) {
	slog.Debug("nodes.Main", "args", args, "os.Args", os.Args)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mount":
			slog.Debug("helper", "args", os.Args)
			os.Exit(0)
		}
	}

	flags := pflag.NewFlagSet("", pflag.ContinueOnError)

	nodeName := flags.String("name", "", "node name")
	rootDir := flags.String("root", "", "root directory for this node")
	kubeConfig := flags.String("kubeconfig", "", "kubeconfig for this node")
	kubeletBinary := flags.String("kubelet", "", "path to the kubelet binary")
	machineID := flags.String("machineID", "", "machine uuid")
	flags.Parse(args)

	n := &Node{
		Name:          *nodeName,
		Root:          *rootDir,
		Kubeconfig:    *kubeConfig,
		KubeletBinary: *kubeletBinary,
		MachineID:     *machineID,
	}

	cmd := flags.Arg(0)
	if cmd == "" {
		cmd = "sandbox"
	}

	switch cmd {
	case "sandbox":
		fatally(ctx, "n.Run", n.RunSandbox)
	case "run":
		fatally(ctx, "n.Init", n.Init)
		fatally(ctx, "n.Run", n.Run)
	case "kubelet":
		n.Kubelet().Exec(ctx)
	case "criserver":
		fatally(ctx, "n.RunCRIServer", n.RunCRIServer)
	default:
		slog.Error("nodes.Main unknown command", "cmd", cmd, "args", args)
		os.Exit(1)
	}
}

func fatally(ctx context.Context, tag string, f func(ctx context.Context) error) {
	if err := f(ctx); err != nil {
		slog.Error(tag, "err", err)
		os.Exit(1)
	}
}
