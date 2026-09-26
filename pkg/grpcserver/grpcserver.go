// Package grpcserver provides a reusable gRPC server that logs with slog and
// shuts down gracefully on context cancellation or termination signals.
package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os/signal"
	"syscall"
	"time"

	"github.com/hoskeri/runkube/pkg/grpcmiddleware"
	"google.golang.org/grpc"
)

// ShutdownTimeout bounds how long a graceful shutdown waits for in-flight RPCs
// before the server is stopped forcefully.
const ShutdownTimeout = 2 * time.Second

// Config describes a gRPC server.
type Config struct {
	// Name identifies the server in log records.
	Name string

	// Network is the network passed to net.Listen ("unix" or "tcp").
	Network string

	// Address is the address passed to net.Listen. For abstract unix sockets
	// use the "@name" form.
	Address string

	// Register is invoked after the underlying *grpc.Server is created and
	// before it starts serving. It is the hook for service registration; a
	// non-nil error aborts startup.
	Register func(*grpc.Server) error

	// Options are appended to the default server options (panic recovery and
	// structured request logging).
	Options []grpc.ServerOption
}

// Server is a gRPC server that logs via slog and shuts down gracefully.
type Server struct {
	cfg Config
	srv *grpc.Server
}

// New builds a Server from cfg without starting it. Structured request logging
// and panic recovery are enabled by default.
func New(cfg Config) *Server {
	opts := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(grpcmiddleware.Recover, grpcmiddleware.Logging),
	}
	opts = append(opts, cfg.Options...)

	return &Server{
		cfg: cfg,
		srv: grpc.NewServer(opts...),
	}
}

// ListenAndServe listens on the configured network/address, registers the
// services, and serves until ctx is cancelled, a termination signal is
// received, or serving fails. Shutdown is graceful and bounded by
// ShutdownTimeout.
func (s *Server) ListenAndServe(ctx context.Context) error {
	lis, err := net.Listen(s.cfg.Network, s.cfg.Address)
	if err != nil {
		return err
	}

	if s.cfg.Register != nil {
		if err := s.cfg.Register(s.srv); err != nil {
			lis.Close()
			return err
		}
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("grpc.server listening",
			"name", s.cfg.Name,
			"network", s.cfg.Network,
			"addr", lis.Addr().String(),
		)
		errCh <- s.srv.Serve(lis)
	}()

	select {
	case err := <-errCh:
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	slog.Info("grpc.server shutting down", "name", s.cfg.Name)
	return s.shutdown()
}

// shutdown drains in-flight RPCs, forcing the server to stop after
// ShutdownTimeout.
func (s *Server) shutdown() error {
	done := make(chan struct{})
	go func() {
		s.srv.GracefulStop()
		close(done)
	}()

	select {
	case <-done:
		slog.Info("grpc.server stopped", "name", s.cfg.Name)
		return nil
	case <-time.After(ShutdownTimeout):
		slog.Warn("grpc.server graceful shutdown timed out",
			"name", s.cfg.Name,
			"timeout", ShutdownTimeout,
		)
		s.srv.Stop()
		<-done
		return nil
	}
}
