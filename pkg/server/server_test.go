package server

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureHandler records every record it is given.
type captureHandler struct {
	records *[]slog.Record
}

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}

func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }

// TestWithLoggerContext verifies that WithLogger attaches a component logger
// to the request context and FromContext returns it.
func TestWithLoggerContext(t *testing.T) {
	var records []slog.Record
	logger := slog.New(captureHandler{records: &records})

	var got *slog.Logger
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = FromContext(r.Context())
		got.Info("component")
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	WithLogger(logger, h).ServeHTTP(httptest.NewRecorder(), req)

	if got != logger {
		t.Errorf("FromContext: got %p, want %p", got, logger)
	}
	if len(records) != 1 || records[0].Message != "component" {
		t.Fatalf("captured records: got %+v", records)
	}
}

// TestFromContextDefault verifies the fallback when no logger is attached.
func TestFromContextDefault(t *testing.T) {
	if FromContext(context.Background()) == nil {
		t.Fatal("FromContext(background) returned nil, want slog.Default() fallback")
	}
}

// TestWithLoggingUsesLogger verifies the request middleware logs through the
// supplied component logger rather than the process default.
func TestWithLoggingUsesLogger(t *testing.T) {
	var records []slog.Record
	logger := slog.New(captureHandler{records: &records})

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/path", nil)
	req.TLS = &tls.ConnectionState{ServerName: "backend.example"}
	WithLogging(logger, inner).ServeHTTP(httptest.NewRecorder(), req)

	if len(records) != 1 {
		t.Fatalf("expected 1 request record, got %d", len(records))
	}
	if records[0].Message != "http.request" {
		t.Errorf("message: got %q, want http.request", records[0].Message)
	}
}
