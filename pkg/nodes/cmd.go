package nodes

import (
	"context"
	"log/slog"
	"os"
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

	// The node configuration arrives from the parent control plane as a
	// serialized environment variable (see Node.EnvEntry) instead of command
	// line flags.
	n, err := NodeFromEnv()
	if err != nil {
		slog.Error("nodes.Main", "err", err)
		os.Exit(1)
	}
	if err := n.Validate(); err != nil {
		slog.Error("nodes.Main", "err", err)
		os.Exit(1)
	}

	var cmd = ""
	if len(args) > 0 {
		cmd = args[0]
	}
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
