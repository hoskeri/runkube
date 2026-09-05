package config

import (
	"log/slog"
	"testing"

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
		name    string
		in      string
		defLvl  slog.Level
		over    map[string]slog.Level
	}{{
		name:    "bare default",
		in:      "debug",
		defLvl:  slog.LevelDebug,
	}, {
		name:    "pairs only",
		in:      "api=debug,node-a=warn",
		defLvl:  slog.LevelInfo, // zero value: no bare level given
		over:    map[string]slog.Level{"api": slog.LevelDebug, "node-a": slog.LevelWarn},
	}, {
		name:    "default and overrides",
		in:      "info,etcd=error,controller=debug",
		defLvl:  slog.LevelInfo,
		over:    map[string]slog.Level{"etcd": slog.LevelError, "controller": slog.LevelDebug},
	}, {
		name:    "spaces tolerated",
		in:      " warn , api = debug ",
		defLvl:  slog.LevelWarn,
		over:    map[string]slog.Level{"api": slog.LevelDebug},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Config{}
			if err := parseLogLevels(c, tt.in); err != nil {
				t.Fatalf("parseLogLevels(%q): %v", tt.in, err)
			}
			if c.LogLevel != tt.defLvl {
				t.Errorf("LogLevel = %v, want %v", c.LogLevel, tt.defLvl)
			}
			if len(c.LogLevels) != len(tt.over) {
				t.Fatalf("LogLevels = %v, want %v", c.LogLevels, tt.over)
			}
			for tag, want := range tt.over {
				if got := c.LogLevels[tag]; got != want {
					t.Errorf("LogLevels[%q] = %v, want %v", tag, got, want)
				}
			}
		})
	}
}

func TestParseLogLevelsErrors(t *testing.T) {
	for _, in := range []string{"", "verbose", "api=verbose", ",,", "=debug"} {
		if err := parseLogLevels(&Config{}, in); err == nil {
			t.Errorf("parseLogLevels(%q) succeeded, want error", in)
		}
	}
}
