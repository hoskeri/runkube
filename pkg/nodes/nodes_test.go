package nodes

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

// sample returns a well-formed Node as produced by parent-side construction
// (see ControlPlane.node in controlplane.go).
func sample() *Node {
	return &Node{
		Name:          "node-a",
		MachineID:     "32e825d5701d11f08f966c02e06b743e",
		Root:          "/",
		Kubeconfig:    "/etc/kubernetes/kubelet.kubeconfig",
		KubeletBinary: "/usr/bin/kubelet",
		LogLevel:      slog.LevelError,
	}
}

// TestEncodeDecodeRoundtrip verifies that serialization preserves every field.
func TestEncodeDecodeRoundtrip(t *testing.T) {
	n := sample()
	b, err := encodeNode(n)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeNode(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != n.Name || got.MachineID != n.MachineID || got.Root != n.Root ||
		got.Kubeconfig != n.Kubeconfig || got.KubeletBinary != n.KubeletBinary ||
		got.LogLevel != n.LogLevel {
		t.Errorf("roundtrip mismatch")
	}
}

// TestEnvEntryRoundtrip verifies that the environment entry produced by
// EnvEntry can be read back by NodeFromEnv.
func TestEnvEntryRoundtrip(t *testing.T) {
	n := sample()
	entry, err := n.EnvEntry()
	if err != nil {
		t.Fatal(err)
	}

	prefix := NodeEnvVar + "="
	if !strings.HasPrefix(entry, prefix) {
		t.Errorf("EnvEntry %q does not start with %q", entry, prefix)
	}

	if err := os.Setenv(NodeEnvVar, entry[len(prefix):]); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv(NodeEnvVar)

	got, err := NodeFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != n.Name || got.MachineID != n.MachineID || got.Root != n.Root ||
		got.Kubeconfig != n.Kubeconfig || got.KubeletBinary != n.KubeletBinary ||
		got.LogLevel != n.LogLevel {
		t.Errorf("NodeFromEnv mismatch")
	}
}

// TestValidateOK verifies that a well-formed node passes validation.
func TestValidateOK(t *testing.T) {
	if err := sample().Validate(); err != nil {
		t.Errorf("valid node rejected: %v", err)
	}
}

// TestValidateRejectsBrokenNodes verifies that Validate flags every way a node
// can be incomplete or malformed, so a dropped field can never slip through
// silently again.
func TestValidateRejectsBrokenNodes(t *testing.T) {
	check := func(mutate func(*Node), want string) {
		t.Helper()
		n := sample()
		mutate(n)
		err := n.Validate()
		if err == nil {
			t.Errorf("Validate accepted node missing %s", want)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate error %q does not mention %s", err.Error(), want)
		}
	}

	check(func(n *Node) { n.Name = "" }, "name")
	check(func(n *Node) { n.MachineID = "" }, "machineID")
	check(func(n *Node) { n.MachineID = "too-short" }, "16 bytes")
	check(func(n *Node) { n.MachineID = "gggggggggggggggggggggggggggggggg" }, "not hex")
	check(func(n *Node) { n.Root = "" }, "root")
	check(func(n *Node) { n.Kubeconfig = "" }, "kubeconfig")
	check(func(n *Node) { n.KubeletBinary = "" }, "kubelet")
	check(func(n *Node) { n.Root = "etc/kubernetes" }, "absolute")
	check(func(n *Node) { n.Kubeconfig = "kubeconfig" }, "absolute")
	check(func(n *Node) { n.KubeletBinary = "kubelet" }, "absolute")
}
