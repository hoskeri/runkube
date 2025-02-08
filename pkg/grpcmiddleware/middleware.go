package grpcmiddleware

import (
	"context"
	"log/slog"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// Logging is a unary interceptor that logs the gRPC call.
func Logging(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	resp, err = handler(ctx, req)
	slog.Debug("grpc.server", "call", info.FullMethod, "req", req, "err", err)
	return
}
