package config

import (
	"log/slog"
	"testing"

	"github.com/hoskeri/procman/pkg/process"
	"github.com/hoskeri/runkube/pkg/randid"
)

func TestGenerateULA(t *testing.T) {
	tests := []struct {
		name string
		seed string
		want string
	}{
		{
			name: "default",
			seed: "default",
			want: "fd37:a8ee:c1ce:1968::/64",
		},
		{
			name: "another",
			seed: "another",
			want: "fdae:448a:c86c:4e8e::/64",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := randid.ULA(tt.seed)
			if got.String() != tt.want {
				t.Errorf("ULA() = %v, want %v", got, tt.want)
			}
			if !got.Addr().Is6() {
				t.Errorf("ULA() = %v, not an IPv6 address", got)
			}
			if got.Addr().As16()[0] != 0xfd {
				t.Errorf("ULA() = %v, does not start with 0xfd", got)
			}
		})
	}
}

func TestGenerateULADifferent(t *testing.T) {
	ula1 := randid.ULA("cluster1")
	ula2 := randid.ULA("cluster2")
	if ula1 == ula2 {
		t.Errorf("ULA() produced same ULA for different seeds: %v", ula1)
	}
}

func TestParseLogLevels(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		def    slog.Level
		defLvl slog.Level
		over   map[string]slog.Level
	}{{
		name:   "bare default",
		in:     "debug",
		def:    slog.LevelError,
		defLvl: slog.LevelDebug,
	}, {
		name:   "pairs only keep flag default",
		in:     "api=debug,node-a=warn",
		def:    slog.LevelError,
		defLvl: slog.LevelError, // no bare level given -> flag default
		over:   map[string]slog.Level{"api": slog.LevelDebug, "node-a": slog.LevelWarn},
	}, {
		name:   "default and overrides",
		in:     "info,etcd=error,controller=debug",
		def:    slog.LevelError,
		defLvl: slog.LevelInfo,
		over:   map[string]slog.Level{"etcd": slog.LevelError, "controller": slog.LevelDebug},
	}, {
		name:   "spaces tolerated",
		in:     " warn , api = debug ",
		def:    slog.LevelError,
		defLvl: slog.LevelWarn,
		over:   map[string]slog.Level{"api": slog.LevelDebug},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := process.ParseLogLevels(tt.in, tt.def)
			if err != nil {
				t.Fatalf("ParseLogLevels(%q): %v", tt.in, err)
			}
			if got.Default != tt.defLvl {
				t.Errorf("Default = %v, want %v", got.Default, tt.defLvl)
			}
			if len(got.Tags) != len(tt.over) {
				t.Fatalf("Tags = %v, want %v", got.Tags, tt.over)
			}
			for tag, want := range tt.over {
				if got := got.Tags[tag]; got != want {
					t.Errorf("Tags[%q] = %v, want %v", tag, got, want)
				}
			}
		})
	}
}

func TestParseLogLevelsErrors(t *testing.T) {
	for _, in := range []string{"", "verbose", "api=verbose", ",,", "=debug"} {
		if _, err := process.ParseLogLevels(in, slog.LevelError); err == nil {
			t.Errorf("ParseLogLevels(%q) succeeded, want error", in)
		}
	}
}
