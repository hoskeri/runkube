package grpcserver

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestListenAndServeGracefulShutdown(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "test.sock")

	s := New(Config{
		Name:    "test",
		Network: "unix",
		Address: sock,
		Register: func(r *grpc.Server) error {
			healthpb.RegisterHealthServer(r, health.NewServer())
			return nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- s.ListenAndServe(ctx)
	}()

	dial := func() *grpc.ClientConn {
		conn, err := grpc.NewClient(
			"passthrough:///unix",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			}),
		)
		if err != nil {
			t.Fatalf("grpc.NewClient: %v", err)
		}
		return conn
	}

	// Wait for the socket to accept connections.
	conn := dial()
	defer conn.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became healthy: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe returned error: %v", err)
		}
	case <-time.After(ShutdownTimeout + 2*time.Second):
		t.Fatal("ListenAndServe did not return after context cancellation")
	}
}
