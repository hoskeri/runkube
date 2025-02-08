package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hoskeri/runkube"
	"github.com/hoskeri/runkube/pkg/config"
	"github.com/hoskeri/runkube/pkg/nodes"
	"github.com/hoskeri/runkube/pkg/sandbox"
)

func main() {
	sandbox.MustInit()

	ctx := context.Background()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: false,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey, slog.LevelKey:
				return slog.Attr{}
			}
			return a
		},
	})))

	if os.Args[0] == "/usr/bin/breakbulk" {
		nodes.Main(ctx, os.Args[1:]...)
	}

	conf, args := config.Parse()
	c := &runkube.ControlPlane{
		Config: conf,
	}

	var cmd = "run"
	if len(args) >= 1 {
		cmd = args[0]
	}

	switch cmd {
	case "run":
		fatally(ctx, "c.Init", c.Init)
		fatally(ctx, "c.Run", c.Run)
	case "apiserver":
		c.APIServer().Exec(ctx)
	case "etcd":
		c.Etcd().Exec(ctx)
	case "controller":
		c.Controller().Exec(ctx)
	case "scheduler":
		c.Scheduler().Exec(ctx)
	case "kms":
		fatally(ctx, "c.RunKMSPlugin", c.RunKMSPlugin)
	case "jwtsigner":
		fatally(ctx, "c.RunJWTSigner", c.RunJWTSigner)
	case "webhook":
		fatally(ctx, "c.RunWebhook", c.RunWebhook)
	case "node":
		fatally(ctx, "c.RunNode", func(ctx context.Context) error {
			if len(args) != 2 {
				return fmt.Errorf("requires node name")
			}

			return c.RunNode(ctx, args[1])
		})
	case "kubectl":
		if err := c.KubectlExec(os.Environ(), args); err != nil {
			os.Exit(1)
		}
	case "curl":
		if err := c.CurlExec(os.Environ(), args[1:]); err != nil {
			os.Exit(1)
		}
	case "etcdctl":
		if err := c.EtcdCtlExec(os.Environ(), args); err != nil {
			os.Exit(1)
		}
	default:
		slog.Error("unknown command", "cmd", cmd, "args", args)
		os.Exit(1)
	}
}

func fatally(ctx context.Context, tag string, f func(ctx context.Context) error) {
	if err := f(ctx); err != nil {
		slog.Error(tag, "err", err)
		os.Exit(1)
	}
}
