package main

import (
	"log/slog"
	"os"

	"github.com/hoskeri/runkube/pkg/sandbox"
)

func main() {
	sandbox.MustInit()

	c := &sandbox.Config{
		Version:   "v1",
		Hostname:  "sandbox",
		MachineID: "32e825d5701d11f08f966c02e06b743e",
		Command:   []string{"/usr/bin/busybox", "sh"},
		IPAddress: "10.0.0.1",
		Env:       []string{},
		Payload: []sandbox.Payload{
			{Target: "/usr/bin/busybox", Source: sandbox.PayloadSource{Type: "bind", Value: "/usr/bin/busybox"}},
		},
	}

	s, err := sandbox.New(c)
	if err != nil {
		slog.Error("sandbox.Run", "err", err)
		os.Exit(1)
	}

	if err := s.Run(); err != nil {
		slog.Error("sandbox.Run", "err", err)
		os.Exit(1)
	}
}
