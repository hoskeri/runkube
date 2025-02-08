package config

import (
	"testing"
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
			got := generateULA(tt.seed)
			if got.String() != tt.want {
				t.Errorf("generateULA() = %v, want %v", got, tt.want)
			}
			if !got.Addr().Is6() {
				t.Errorf("generateULA() = %v, not an IPv6 address", got)
			}
			if got.Addr().As16()[0] != 0xfd {
				t.Errorf("generateULA() = %v, does not start with 0xfd", got)
			}
		})
	}
}

func TestGenerateULADifferent(t *testing.T) {
	ula1 := generateULA("cluster1")
	ula2 := generateULA("cluster2")
	if ula1 == ula2 {
		t.Errorf("generateULA() produced same ULA for different seeds: %v", ula1)
	}
}
