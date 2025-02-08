package procfs

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFakeProcFS(t *testing.T) {
	// Create mock real proc directory
	tmpDir, err := os.MkdirTemp("", "fake-procfs-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	realProc := filepath.Join(tmpDir, "real_proc")
	mountPoint := filepath.Join(tmpDir, "mount")

	if err := os.MkdirAll(realProc, 0755); err != nil {
		t.Fatalf("failed to create real_proc dir: %v", err)
	}
	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		t.Fatalf("failed to create mountPoint dir: %v", err)
	}

	// Create some mock files in real proc
	if err := os.WriteFile(filepath.Join(realProc, "cpuinfo"), []byte("real cpuinfo\n"), 0644); err != nil {
		t.Fatalf("failed to write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realProc, "meminfo"), []byte("real meminfo\n"), 0644); err != nil {
		t.Fatalf("failed to write meminfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realProc, "version"), []byte("real version\n"), 0644); err != nil {
		t.Fatalf("failed to write version: %v", err)
	}
	// Create fake "self" and "thread-self" symlinks in real proc so they show up in readdir
	if err := os.Symlink("self-target-placeholder", filepath.Join(realProc, "self")); err != nil {
		t.Fatalf("failed to create self symlink: %v", err)
	}
	if err := os.Symlink("thread-self-target-placeholder", filepath.Join(realProc, "thread-self")); err != nil {
		t.Fatalf("failed to create thread-self symlink: %v", err)
	}

	// Configure options with overrides
	opts := &Options{
		RealProcPath: realProc,
		Overrides: map[string]func(ctx context.Context) ([]byte, error){
			"cpuinfo": func(ctx context.Context) ([]byte, error) {
				return []byte("fake cpuinfo override\n"), nil
			},
			"meminfo": func(ctx context.Context) ([]byte, error) {
				return []byte("fake meminfo override\n"), nil
			},
		},
	}

	// Attempt mount
	_, err = Mount(mountPoint, opts)
	if err != nil {
		t.Fatalf("failed to mount: %v", err)
	}

	// Wait briefly for mount to be ready
	time.Sleep(100 * time.Millisecond)

	// Test overridden files
	gotCpuinfo, err := os.ReadFile(filepath.Join(mountPoint, "cpuinfo"))
	if err != nil {
		t.Errorf("failed to read cpuinfo: %v", err)
	} else if string(gotCpuinfo) != "fake cpuinfo override\n" {
		t.Errorf("expected cpuinfo override, got %q", string(gotCpuinfo))
	}

	gotMeminfo, err := os.ReadFile(filepath.Join(mountPoint, "meminfo"))
	if err != nil {
		t.Errorf("failed to read meminfo: %v", err)
	} else if string(gotMeminfo) != "fake meminfo override\n" {
		t.Errorf("expected meminfo override, got %q", string(gotMeminfo))
	}

	// Test non-overridden files (should delegate to real procfs)
	gotVersion, err := os.ReadFile(filepath.Join(mountPoint, "version"))
	if err != nil {
		t.Errorf("failed to read version: %v", err)
	} else if string(gotVersion) != "real version\n" {
		t.Errorf("expected real version, got %q", string(gotVersion))
	}

	// Test "self" symlink
	gotSelfLink, err := os.Readlink(filepath.Join(mountPoint, "self"))
	if err != nil {
		t.Errorf("failed to readlink self: %v", err)
	} else {
		// Check that it's a valid pid
		pid, err := strconv.Atoi(gotSelfLink)
		if err != nil || pid <= 0 {
			t.Errorf("expected self to point to a valid pid, got %q", gotSelfLink)
		}
	}

	// Test "thread-self" symlink
	gotThreadSelfLink, err := os.Readlink(filepath.Join(mountPoint, "thread-self"))
	if err != nil {
		t.Errorf("failed to readlink thread-self: %v", err)
	} else if !strings.HasPrefix(gotThreadSelfLink, "self/task/") {
		t.Errorf("expected thread-self to point to self/task/<tid>, got %q", gotThreadSelfLink)
	}
}

func TestStaticFileHandle_Read(t *testing.T) {
	fh := &staticFileHandle{content: []byte("hello world")}
	dest := make([]byte, 5)

	res, err := fh.Read(context.Background(), dest, 0)
	if err != 0 {
		t.Fatalf("unexpected read error: %v", err)
	}

	// ReadResult interface provides Bytes(buf []byte) ([]byte, Status)
	buf := make([]byte, res.Size())
	data, status := res.Bytes(buf)
	if !status.Ok() {
		t.Fatalf("failed to get bytes from ReadResult: %v", status)
	}
	if string(data) != "hello" {
		t.Errorf("expected 'hello', got %q", string(data))
	}
}
