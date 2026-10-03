package runkube

import (
	"slices"
	"testing"

	"github.com/hoskeri/runkube/pkg/config"
)

// TestChildSubcommandsInheritConfig guards against re-executed runkube
// subcommands (kms, jwtsigner, webhook) re-parsing argv and silently falling
// back to cluster name "default" and the default root. Their command lines must
// carry the parent's identity so a non-default cluster works at all.
func TestChildSubcommandsInheritConfig(t *testing.T) {
	c := &ControlPlane{Config: config.Config{
		Name: "mytest",
		Root: "/var/lib/runkube/mytest",
	}}

	for _, tc := range []struct {
		tag  string
		args []string
	}{
		{"kms", c.KMSPlugin().CmdArgs},
		{"jwtsigner", c.JWTSigner().CmdArgs},
		{"webhook", c.Webhook().CmdArgs},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			i := slices.Index(tc.args, "--name")
			if i < 0 || i+1 >= len(tc.args) || tc.args[i+1] != "mytest" {
				t.Fatalf("--name mytest missing from %v", tc.args)
			}
			i = slices.Index(tc.args, "--root")
			if i < 0 || i+1 >= len(tc.args) || tc.args[i+1] != "/var/lib/runkube/mytest" {
				t.Fatalf("--root /var/lib/runkube/mytest missing from %v", tc.args)
			}
		})
	}
}
