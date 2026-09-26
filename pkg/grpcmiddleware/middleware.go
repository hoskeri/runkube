package grpcmiddleware

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Recover is a unary interceptor that recovers from panics and returns an internal server error.
func Recover(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug("Recovered from panic: %v\nStack trace:\n%s", r, debug.Stack())
			err = status.Errorf(codes.Internal, "internal server error: %v", r)
		}
	}()

	return handler(ctx, req)
}

// Logging is a unary interceptor that logs the gRPC call, mirroring the HTTP
// request logging in pkg/server: successful calls log at debug, everything
// else at info.
func Logging(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	start := time.Now()
	resp, err = handler(ctx, req)
	duration := time.Since(start)

	code := status.Code(err)

	lvl := slog.LevelInfo
	if code == codes.OK {
		lvl = slog.LevelDebug
	}

	attrs := []any{
		"m", info.FullMethod,
		"status", code.String(),
		"duration", duration,
		"rq.len", msgSize(req),
		"rs.len", msgSize(resp),
	}
	if p, ok := peer.FromContext(ctx); ok {
		attrs = append(attrs, "peer", p.Addr.String())
	}

	slog.Log(ctx, lvl, "grpc.request", attrs...)
	return
}

// msgSize returns the protobuf wire size of m, or 0 if m is not a proto
// message. Only the size is logged, never the body, so request payloads (e.g.
// KMS plaintext) are not written to logs.
func msgSize(m any) int {
	pm, ok := m.(proto.Message)
	if !ok {
		return 0
	}
	return proto.Size(pm)
}
