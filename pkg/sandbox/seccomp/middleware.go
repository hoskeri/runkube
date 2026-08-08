package seccomp

import (
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hoskeri/runkube/pkg/sandbox/seccomp/internal"
)

// LoggingMiddleware logs all intercepted syscalls
func LoggingMiddleware() internal.Middleware {
	return func(next internal.Handler) internal.Handler {
		return internal.HandlerFunc(func(ctx *internal.Context) {
			slog.Debug("seccomp intercept", "method", ctx.Method, "path", ctx.Path, "pid", ctx.PID, "syscall", ctx.Syscall)
			next.ServeSeccomp(ctx)
		})
	}
}

// MetricsMiddleware records metrics (placeholder for actual metrics)
func MetricsMiddleware() internal.Middleware {
	return func(next internal.Handler) internal.Handler {
		return internal.HandlerFunc(func(ctx *internal.Context) {
			start := time.Now()
			next.ServeSeccomp(ctx)
			duration := time.Since(start)
			_ = duration // TODO: integrate with metrics package
			// metrics.Record(ctx.Method, ctx.Path, duration)
		})
	}
}

// PathRewriteMiddleware rewrites paths before handler
func PathRewriteMiddleware(rewrites map[string]string) internal.Middleware {
	return func(next internal.Handler) internal.Handler {
		return internal.HandlerFunc(func(ctx *internal.Context) {
			if newPath, ok := rewrites[ctx.Path]; ok {
				ctx.Path = newPath
			}
			next.ServeSeccomp(ctx)
		})
	}
}

// PathPrefixRewriteMiddleware rewrites path prefixes
func PathPrefixRewriteMiddleware(rewrites map[string]string) internal.Middleware {
	// Sort prefixes by length (longest first)
	var prefixes []string
	for p := range rewrites {
		prefixes = append(prefixes, p)
	}
	// Simple bubble sort by length desc (small n, so fine)
	for i := 0; i < len(prefixes); i++ {
		for j := i + 1; j < len(prefixes); j++ {
			if len(prefixes[j]) > len(prefixes[i]) {
				prefixes[i], prefixes[j] = prefixes[j], prefixes[i]
			}
		}
	}

	return func(next internal.Handler) internal.Handler {
		return internal.HandlerFunc(func(ctx *internal.Context) {
			for _, prefix := range prefixes {
				if strings.HasPrefix(ctx.Path, prefix) {
					ctx.Path = rewrites[prefix] + ctx.Path[len(prefix):]
					break
				}
			}
			next.ServeSeccomp(ctx)
		})
	}
}

// ConditionalMiddleware only applies middleware if condition matches
func ConditionalMiddleware(cond func(*internal.Context) bool, mw internal.Middleware) internal.Middleware {
	return func(next internal.Handler) internal.Handler {
		return internal.HandlerFunc(func(ctx *internal.Context) {
			if cond(ctx) {
				mw(next).ServeSeccomp(ctx)
			} else {
				next.ServeSeccomp(ctx)
			}
		})
	}
}

// RateLimitMiddleware limits calls per second per path
func RateLimitMiddleware(maxPerSecond int) internal.Middleware {
	type bucket struct {
		count     int
		lastReset time.Time
	}
	buckets := make(map[string]*bucket)

	return func(next internal.Handler) internal.Handler {
		return internal.HandlerFunc(func(ctx *internal.Context) {
			now := time.Now()
			key := ctx.Path
			b := buckets[key]
			if b == nil || now.Sub(b.lastReset) >= time.Second {
				buckets[key] = &bucket{count: 1, lastReset: now}
				next.ServeSeccomp(ctx)
				return
			}
			if b.count >= maxPerSecond {
				ctx.Respond.Error(unix.EAGAIN)
				return
			}
			b.count++
			next.ServeSeccomp(ctx)
		})
	}
}

// AuditMiddleware logs sensitive path accesses
func AuditMiddleware() internal.Middleware {
	return func(next internal.Handler) internal.Handler {
		return internal.HandlerFunc(func(ctx *internal.Context) {
			slog.Warn("seccomp audit", "method", ctx.Method, "path", ctx.Path, "pid", ctx.PID)
			next.ServeSeccomp(ctx)
		})
	}
}

// MethodFilterMiddleware only allows certain methods
func MethodFilterMiddleware(allowedMethods ...string) internal.Middleware {
	allowed := make(map[string]bool)
	for _, m := range allowedMethods {
		allowed[m] = true
	}
	return func(next internal.Handler) internal.Handler {
		return internal.HandlerFunc(func(ctx *internal.Context) {
			if allowed[ctx.Method] {
				next.ServeSeccomp(ctx)
			} else {
				ctx.Respond.Error(unix.EPERM)
			}
		})
	}
}

// PathFilterMiddleware only allows paths matching predicate
func PathFilterMiddleware(allow func(path string) bool) internal.Middleware {
	return func(next internal.Handler) internal.Handler {
		return internal.HandlerFunc(func(ctx *internal.Context) {
			if allow(ctx.Path) {
				next.ServeSeccomp(ctx)
			} else {
				ctx.Respond.Continue()
			}
		})
	}
}

// ChainMiddlewares applies multiple middlewares in order
func ChainMiddlewares(mws ...internal.Middleware) internal.Middleware {
	return func(next internal.Handler) internal.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			next = mws[i](next)
		}
		return next
	}
}