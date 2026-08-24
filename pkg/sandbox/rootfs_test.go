package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// openTestRootfs opens a scratch rootfs directory for tests.
func openTestRootfs(t *testing.T) (*Rootfs, string) {
	t.Helper()
	dir := t.TempDir()
	f, err := os.OpenFile(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return NewRootfs(f), dir
}

func TestRootfsWriteCreatesParentsAndContent(t *testing.T) {
	r, dir := openTestRootfs(t)
	content := "Node 0 MemTotal:       1048576 kB\n"
	if err := r.Write("/sys/devices/system/node/node0/meminfo", []byte(content), 0444); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sys/devices/system/node/node0/meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("content = %q, want %q", got, content)
	}
	fi, err := os.Stat(filepath.Join(dir, "sys/devices/system/node/node0/meminfo"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0444 {
		t.Errorf("mode = %v, want 0444", fi.Mode().Perm())
	}
}

func TestRootfsWriteReplacesAndTruncates(t *testing.T) {
	r, dir := openTestRootfs(t)
	if err := r.Write("/a/b", []byte("longer content that must be truncated"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("/a/b", []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a/b"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "x" {
		t.Errorf("content = %q, want %q", got, "x")
	}
}

// TestRootfsWriteOverReadOnlyFile is the regression test for the rename-based
// write: updating a read-only (0444) file must succeed through the rootfs
// handle even though a direct O_WRONLY open would fail. This is what lets the
// parent rewrite the model's sysfs tree at runtime, and lets child and parent
// materialize the same files concurrently.
func TestRootfsWriteOverReadOnlyFile(t *testing.T) {
	r, dir := openTestRootfs(t)
	p := filepath.Join(dir, "sys/devices/system/cpu/online")
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("0-3\n"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("/sys/devices/system/cpu/online", []byte("0-7\n"), 0444); err != nil {
		t.Fatalf("rewrite over read-only file: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0-7\n" {
		t.Errorf("content = %q, want %q", got, "0-7\n")
	}
	if unix.Geteuid() != 0 {
		if err := os.WriteFile(p, []byte("0-1\n"), 0444); err == nil {
			t.Error("direct write over read-only file should fail for non-root")
		}
	}
}

func TestRootfsWriteEmptyContent(t *testing.T) {
	r, dir := openTestRootfs(t)
	// The sysfs "offline" file is modeled with empty content; it must exist
	// as a zero-length file.
	if err := r.Write("/sys/devices/system/cpu/offline", nil, 0444); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "sys/devices/system/cpu/offline")
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 {
		t.Errorf("size = %d, want 0", fi.Size())
	}
}

func TestRootfsMkdirAllIdempotent(t *testing.T) {
	r, dir := openTestRootfs(t)
	for i := 0; i < 2; i++ {
		if err := r.MkdirAll("/sys/devices/system/node/node0", 0755); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "sys/devices/system/node/node0")); err != nil || !fi.IsDir() {
		t.Errorf("dir not created: %v %v", fi, err)
	}
}

func TestRootfsPathValidation(t *testing.T) {
	r, dir := openTestRootfs(t)

	// Relative paths are rejected: the *at syscalls would resolve them
	// against the process CWD, not the rootfs.
	if err := r.Write("relative/path", []byte("x"), 0644); err == nil {
		t.Error("relative path accepted")
	}

	// Absolute paths with ".." are clamped to the root by path.Clean, so
	// they can never escape into the host filesystem.
	if err := r.Write("/../../escape", []byte("x"), 0644); err != nil {
		t.Errorf("clamped absolute path rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape")); err != nil {
		t.Errorf("clamped path not written under rootfs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); !os.IsNotExist(err) {
		t.Errorf("path escaped the rootfs: %v", err)
	}
}

func TestRootfsWriteRootPathRejected(t *testing.T) {
	r, _ := openTestRootfs(t)
	if err := r.Write("/", []byte("x"), 0644); err == nil {
		t.Error("writing the root itself should fail")
	}
}

func TestBootFiles(t *testing.T) {
	sys := testSystem() // 8 threads
	s := &Sandbox{config: &Config{
		Version: "v1",
		System:  sys,
		Payload: []Payload{
			{Target: "/usr/bin/mount", FileMode: 0755, Source: PayloadSource{Type: "write", Value: "#!/bin/sh true\n"}},
			{Target: "/etc/no-newline", Source: PayloadSource{Type: "write", Value: "abc"}},
			{Target: "/usr/bin/passwd", FileMode: 0644, Source: PayloadSource{Type: "write", Value: "xyz\n"}},
			{Target: "/usr/bin/kubelet", Source: PayloadSource{Type: "bind", Value: "/host/kubelet"}},
			{Target: "/run/ignored", Source: PayloadSource{Type: "mystery", Value: "?"}},
		},
	}}

	want := sys.SysfsDevices()
	files := s.bootFiles()
	if len(files) != len(want)+3 {
		t.Fatalf("bootFiles() = %d entries, want %d", len(files), len(want)+3)
	}

	// The sysfs tree comes first, byte-identical to SysfsDevices.
	for i, f := range want {
		got := files[i]
		if got.path != f.Path || string(got.content) != f.Content {
			t.Errorf("sysfs entry %d = %q %q, want %q %q", i, got.path, got.content, f.Path, f.Content)
		}
		if got.mode.Perm() != 0444 {
			t.Errorf("sysfs entry %d mode = %v, want 0444", i, got.mode.Perm())
		}
	}

	// Write payloads follow in config order; missing newline is appended and
	// a zero FileMode defaults to 0755, mirroring the child's write().
	cases := []struct {
		path    string
		content string
		mode    os.FileMode
	}{
		{"/usr/bin/mount", "#!/bin/sh true\n", 0755},
		{"/etc/no-newline", "abc\n", 0755}, // newline appended; mode defaulted
		{"/usr/bin/passwd", "xyz\n", 0644}, // explicit 0644 preserved
	}
	for i, tc := range cases {
		got := files[len(want)+i]
		if got.path != tc.path || string(got.content) != tc.content || got.mode.Perm() != tc.mode {
			t.Errorf("payload %d = %q %q %v, want %q %q %v", i, got.path, got.content, got.mode, tc.path, tc.content, tc.mode)
		}
	}

	// Bind and unknown payloads are excluded from bootFiles.
	for _, f := range files {
		if f.path == "/usr/bin/kubelet" || f.path == "/run/ignored" {
			t.Errorf("bind/non-write payload leaked into bootFiles: %q", f.path)
		}
	}
}

func TestInjectFilesRuntimeAPI(t *testing.T) {
	r, dir := openTestRootfs(t)
	s := &Sandbox{rootfs: r}
	files := []SysfsFile{
		{Path: "/sys/devices/system/cpu/online", Content: "0-15\n"},
		{Path: "/sys/devices/system/cpu/offline", Content: ""},
	}
	if err := s.InjectFiles(files); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sys/devices/system/cpu/online"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "0-15\n" {
		t.Errorf("online = %q", got)
	}

	// Without a rootfs handle the runtime API fails cleanly.
	unstarted := &Sandbox{}
	if err := unstarted.InjectFiles(files); err == nil || !strings.Contains(err.Error(), "rootfs") {
		t.Errorf("expected rootfs-missing error, got %v", err)
	}
}
