package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// loggerKey is the context key under which a per-component logger travels.
type loggerKey struct{}

// WithContextLogger returns a copy of ctx carrying logger. The frontend uses
// it to attach a backend's component logger to each proxied request, so
// handlers can log through FromContext without every handler type growing a
// logger parameter.
func WithContextLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

// FromContext returns the logger attached to ctx by WithContextLogger, or
// slog.Default() when none is present.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// WithLogger injects logger into the request context before invoking h, so
// the wrapped handler can log through FromContext.
func WithLogger(logger *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h.ServeHTTP(w, req.WithContext(WithContextLogger(req.Context(), logger)))
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	http.Flusher
	http.CloseNotifier

	StatusCode int
	Size       int
}

func (l *loggingResponseWriter) CloseNotify() <-chan bool {
	return l.ResponseWriter.(http.CloseNotifier).CloseNotify()
}

func (l *loggingResponseWriter) Header() http.Header {
	return l.ResponseWriter.Header()
}

func (l *loggingResponseWriter) Written() bool {
	return l.StatusCode != 0
}

func (l *loggingResponseWriter) WriteHeader(status int) {
	if l.Written() {
		return
	}
	l.StatusCode = status
	l.ResponseWriter.WriteHeader(status)
}

func (l *loggingResponseWriter) Write(b []byte) (int, error) {
	if !l.Written() {
		l.WriteHeader(http.StatusOK)
		l.StatusCode = http.StatusOK
	}
	n, err := l.ResponseWriter.Write(b)
	l.Size = l.Size + n
	return n, err
}

func (l *loggingResponseWriter) Flush() {
	flusher, ok := l.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}

	if !l.Written() {
		l.WriteHeader(http.StatusOK)
		l.StatusCode = http.StatusOK
	}

	flusher.Flush()
}

func newLoggingResponseWriter(w http.ResponseWriter) *loggingResponseWriter {
	return &loggingResponseWriter{
		ResponseWriter: w,
	}
}

func WithLogging(logger *slog.Logger, h http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()

		lw := newLoggingResponseWriter(w)
		start := time.Now()

		h.ServeHTTP(lw, req)
		duration := time.Since(start)

		lvl := slog.LevelInfo
		switch lw.StatusCode {
		case http.StatusOK, http.StatusNoContent, 0:
			lvl = slog.LevelDebug
		}

		logger.Log(ctx, lvl, "http.request",
			"m", req.Method,
			"sni", req.TLS.ServerName,
			"host", req.Host,
			"status", lw.StatusCode,
			"duration", duration,
			"path", req.URL.EscapedPath(),
			"rs.len", lw.Size,
		)
	})
}

func WithMatchingHost(logger *slog.Logger, h http.Handler, n string) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	nh := n + ":443"
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if n == "" {
			logger.Error("with-matching-host server name not configured", "host", n)
			http.Error(w, "", http.StatusInternalServerError)
			return
		}

		if req.TLS == nil {
			logger.Error("with-matching-host not tls request", "req.url", req.URL)
			http.Error(w, "", http.StatusBadGateway)
			return
		}

		if req.TLS.ServerName != n {
			logger.Error("with-matching-host sni mismatch", "got", req.TLS.ServerName, "want", n)
			http.Error(w, "", http.StatusBadGateway)
			return
		}

		if !(req.Host == n || req.Host == nh) {
			logger.Error("with-matching-host host header mismatch", "got", req.Host, "want", n)
			http.Error(w, "", http.StatusBadGateway)
			return
		}

		// serve the request.
		h.ServeHTTP(w, req)
	})
}
