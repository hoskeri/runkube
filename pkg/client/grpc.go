package client

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// GRPC is a generic gRPC client with the standard health service.
type GRPC struct {
	conn *grpc.ClientConn
}

// Dial creates a client for target, which uses the gRPC naming syntax
// ("unix:///path", "unix://@abstract", "dns:///host:port", ...), authenticating
// with creds. The connection is established lazily on the first RPC.
func Dial(target string, creds credentials.TransportCredentials) (*GRPC, error) {
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	return &GRPC{conn: conn}, nil
}

// Close releases the underlying connection.
func (c *GRPC) Close() error { return c.conn.Close() }

// HealthCheck calls grpc.health.v1.Health/Check and returns an error unless
// the server reports SERVING. service selects the service to ask about; the
// empty string asks for the whole server's health.
func (c *GRPC) HealthCheck(ctx context.Context, service string) error {
	resp, err := healthpb.NewHealthClient(c.conn).Check(ctx, &healthpb.HealthCheckRequest{Service: service})
	if err != nil {
		return fmt.Errorf("health check: %w", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("health check: serving status %s", resp.GetStatus())
	}
	return nil
}
