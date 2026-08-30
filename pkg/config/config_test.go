package config

import (
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
