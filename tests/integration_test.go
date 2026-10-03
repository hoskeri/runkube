// Package tests holds end-to-end tests that exercise the real runkube binary,
// exactly as a user would run it.
//
// The Makefile builds the binary once into _output/bin/runkube and exports its
// path as RUNKUBE_BIN; `make test-integration` does both. With a bare
// `go test ./...` (RUNKUBE_BIN unset) these tests skip.
//
// The cluster root must live outside /tmp: the sandbox mounts a fresh tmpfs at
// /tmp, so any host path under it is invisible to the sandbox's bind mounts.
// RUNKUBE_ITEST_BASE overrides the temporary-root parent when the default
// (under $HOME/.cache) is unsuitable.
package tests

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// runkubeBin resolves the prebuilt runkube binary, skipping the test when the
// Makefile did not point RUNKUBE_BIN at it.
func runkubeBin(t *testing.T) string {
	t.Helper()
	b := os.Getenv("RUNKUBE_BIN")
	if b == "" {
		t.Skip("RUNKUBE_BIN not set; run `make test-integration` to build the binary and run this test")
	}
	abs, err := filepath.Abs(b)
	if err != nil {
		t.Fatalf("resolve RUNKUBE_BIN: %v", err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("runkube binary %q missing; run `make test-integration`", abs)
	}
	return abs
}

// itestBase returns the directory that cluster roots are created under. It must
// not be /tmp (see the package comment).
func itestBase(t *testing.T) string {
	t.Helper()
	base := os.Getenv("RUNKUBE_ITEST_BASE")
	if base != "" {
		return base
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home directory for integration root: %v", err)
	}
	return filepath.Join(home, ".cache", "runkube-itest")
}

// uniqueName returns a DNS-label-safe cluster name that won't collide with a
// developer's cluster or with another test run.
func uniqueName(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "itest-" + hex.EncodeToString(b[:])
}

// TestClusterBootsWithoutPanic boots a real, fully isolated single-node
// cluster and asserts that it reaches node registration without panicking.
//
// It exists because a startup panic in a node's nested procman formation (a
// nil slog level in FramerHandler) only manifests when the whole cluster is
// launched; no unit test in runkube exercised that path.
func TestClusterBootsWithoutPanic(t *testing.T) {
	bin := runkubeBin(t)

	base := itestBase(t)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("create integration base %q: %v", base, err)
	}
	root, err := os.MkdirTemp(base, "cluster-")
	if err != nil {
		t.Fatalf("create cluster root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	name := uniqueName(t)
	cmd := exec.Command(bin,
		"--name", name,
		"--root", root,
		"--node", "node-a",
		"--log-level", "debug",
	)

	// Both streams share one pipe; the scanner below tees the output into a
	// buffer and watches for the readiness marker and panics as they happen.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		t.Fatalf("start runkube: %v", err)
	}
	// The write end lives only in the child now, so the reader sees EOF when
	// the cluster exits.
	pw.Close()

	var (
		mu  sync.Mutex
		buf bytes.Buffer
	)
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
	ready := make(chan struct{})
	panicked := make(chan struct{})
	var readyOnce, panicOnce sync.Once

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			buf.WriteString(line)
			buf.WriteByte('\n')
			mu.Unlock()
			if strings.Contains(line, "Successfully registered node") {
				readyOnce.Do(func() { close(ready) })
			}
			if strings.Contains(line, "panic:") {
				panicOnce.Do(func() { close(panicked) })
			}
		}
	}()

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		// Best effort: ask nicely first, then force, then sweep any surviving
		// sandbox descendants by their command line (which embeds the root).
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-waitErr:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-waitErr
		}
		killByRoot(root)
	})

	select {
	case <-ready:
		// The cluster is up; fall through to shutdown.
	case <-panicked:
		_ = cmd.Process.Kill()
		<-waitErr
		stopped = true
		killByRoot(root)
		t.Fatalf("runkube panicked during startup:\n%s", tail(snapshot(), 60))
	case err := <-waitErr:
		stopped = true
		out := snapshot()
		if isSandboxUnavailable(out) {
			t.Skipf("sandbox unavailable in this environment; skipping:\n%s", tail(out, 20))
		}
		t.Fatalf("runkube exited before registering a node (err=%v):\n%s", err, tail(out, 60))
	case <-time.After(120 * time.Second):
		_ = cmd.Process.Kill()
		<-waitErr
		stopped = true
		killByRoot(root)
		t.Fatalf("runkube did not register a node within 120s:\n%s", tail(snapshot(), 60))
	}

	// A user-initiated shutdown must be graceful: the process exits 0.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal runkube: %v", err)
	}
	select {
	case <-waitErr:
		stopped = true
		if code := cmd.ProcessState.ExitCode(); code != 0 {
			t.Fatalf("runkube exited %d after SIGTERM, want 0:\n%s", code, tail(snapshot(), 60))
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-waitErr
		stopped = true
		killByRoot(root)
		t.Fatalf("runkube did not exit within 60s of SIGTERM:\n%s", tail(snapshot(), 60))
	}

	<-scanDone
	if out := snapshot(); strings.Contains(out, "panic:") {
		t.Fatalf("runkube output contained a panic:\n%s", tail(out, 60))
	}
}

// isSandboxUnavailable reports whether the output shows the sandbox could not
// be set up at all (e.g. user namespaces disabled), as opposed to a real
// cluster failure. Such environments cannot reproduce the class of bug this
// test guards against, so the test skips instead of reporting a false failure.
func isSandboxUnavailable(out string) bool {
	for _, marker := range []string{
		"sandbox.MustInit error",
		"operation not permitted",
		"clone3: operation not permitted",
	} {
		if strings.Contains(out, marker) {
			return true
		}
	}
	return false
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// killByRoot SIGKILLs every process whose command line mentions root. This
// cleans up sandbox descendants if the cluster had to be killed abruptly
// (procman's children are in their own sessions, so a process-group kill does
// not reach them).
func killByRoot(root string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if bytes.Contains(raw, []byte(root)) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
