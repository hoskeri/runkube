package seccomp

import (
	"testing"

	"golang.org/x/sys/unix"

	"github.com/hoskeri/runkube/pkg/sandbox/seccomp/internal"
)

// fakeResponder implements internal.Responder for unit testing
type fakeResponder struct {
	action   string
	errno    unix.Errno
	fd       int
	statAddr uint64
	statVal  any
	readAt   uint64
	readTo   string
	readSz   uint64
	retVal   int64
}

func (f *fakeResponder) Continue() error { f.action = "continue"; return nil }
func (f *fakeResponder) Error(errno unix.Errno) error {
	f.action = "error"
	f.errno = errno
	return nil
}
func (f *fakeResponder) SpoofFD(fd int) error { f.action = "spoof_fd"; f.fd = fd; return nil }
func (f *fakeResponder) SpoofStat(addr uint64, stat any) error {
	f.action = "spoof_stat"
	f.statAddr = addr
	f.statVal = stat
	return nil
}
func (f *fakeResponder) SpoofReadlink(addr uint64, target string, bufSize uint64) error {
	f.action = "spoof_readlink"
	f.readAt = addr
	f.readTo = target
	f.readSz = bufSize
	return nil
}
func (f *fakeResponder) SpoofUname(addr uint64, uts *unix.Utsname) error {
	f.action = "spoof_uname"
	f.statAddr = addr
	f.statVal = uts
	return nil
}
func (f *fakeResponder) Success(ret int64) error { f.action = "success"; f.retVal = ret; return nil }

func TestMuxExactMatch(t *testing.T) {
	mux := NewMux()
	content := "fake data"
	mux.Handle("OPEN", "/proc/mounts", SpoofFile(content))

	config := mux.Build()

	// Dispatch a matching request
	resp := &fakeResponder{}
	ctx := &internal.Context{
		PID:     42,
		Syscall: unix.SYS_OPENAT,
		Method:  "OPEN",
		Path:    "/proc/mounts",
		Args:    [6]uint64{},
		Respond: resp,
	}
	config.HandlerRegistry.Dispatch(ctx)

	if resp.action != "spoof_fd" {
		t.Errorf("expected spoof_fd action, got %q", resp.action)
	}
}

func TestMuxNoMatchContinues(t *testing.T) {
	mux := NewMux()
	mux.Handle("OPEN", "/proc/mounts", SpoofFile("fake"))

	config := mux.Build()

	resp := &fakeResponder{}
	ctx := &internal.Context{
		Syscall: unix.SYS_OPENAT,
		Method:  "OPEN",
		Path:    "/etc/passwd",
		Respond: resp,
	}
	config.HandlerRegistry.Dispatch(ctx)

	if resp.action != "continue" {
		t.Errorf("expected continue for non-matching path, got %q", resp.action)
	}
}

func TestMuxPrefixMatch(t *testing.T) {
	mux := NewMux()
	mux.HandlePrefix("OPEN", "/fake", SpoofFile("prefix-content"))

	config := mux.Build()

	resp := &fakeResponder{}
	ctx := &internal.Context{
		Syscall: unix.SYS_OPENAT,
		Method:  "OPEN",
		Path:    "/fake/file.txt",
		Respond: resp,
	}
	config.HandlerRegistry.Dispatch(ctx)

	if resp.action != "spoof_fd" {
		t.Errorf("expected spoof_fd for prefix match, got %q", resp.action)
	}
}

func TestMuxCatchAll(t *testing.T) {
	mux := NewMux()
	mux.Handle("OPEN", "*", SpoofFile("catch-all"))

	config := mux.Build()

	resp := &fakeResponder{}
	ctx := &internal.Context{
		Syscall: unix.SYS_OPENAT,
		Method:  "OPEN",
		Path:    "/any/path",
		Respond: resp,
	}
	config.HandlerRegistry.Dispatch(ctx)

	if resp.action != "spoof_fd" {
		t.Errorf("expected spoof_fd for catch-all, got %q", resp.action)
	}
}

func TestMuxWildcardAndStaticCoexist(t *testing.T) {
	mux := NewMux()
	mux.Handle("OPEN", "/proc/mounts", SpoofFile("mounts"))
	mux.Handle("OPEN", "*", SpoofFile("other"))

	config := mux.Build()

	// Static path should match exact route
	resp1 := &fakeResponder{}
	ctx1 := &internal.Context{Syscall: unix.SYS_OPENAT, Method: "OPEN", Path: "/proc/mounts", Respond: resp1}
	config.HandlerRegistry.Dispatch(ctx1)

	// Non-static path should match catch-all
	resp2 := &fakeResponder{}
	ctx2 := &internal.Context{Syscall: unix.SYS_OPENAT, Method: "OPEN", Path: "/etc/hosts", Respond: resp2}
	config.HandlerRegistry.Dispatch(ctx2)

	if resp1.action != "spoof_fd" || resp2.action != "spoof_fd" {
		t.Errorf("expected both to be spoofed, got resp1=%q resp2=%q", resp1.action, resp2.action)
	}
}

func TestSpoofStatHandlesAllMethods(t *testing.T) {
	stat := &unix.Stat_t{Ino: 12345}

	// STAT method (newfstatat: arg[2] = statbuf)
	mux := NewMux()
	mux.Handle("STAT", "/fake/stat", SpoofStat(stat))
	config := mux.Build()

	resp := &fakeResponder{}
	ctx := &internal.Context{
		Syscall: unix.SYS_NEWFSTATAT,
		Method:  "STAT",
		Path:    "/fake/stat",
		Args:    [6]uint64{0, 0, 0xDEAD, 0},
		Respond: resp,
	}
	config.HandlerRegistry.Dispatch(ctx)

	if resp.action != "spoof_stat" {
		t.Errorf("expected spoof_stat, got %q", resp.action)
	}
	if resp.statAddr != 0xDEAD {
		t.Errorf("expected stat addr 0xDEAD, got %#x", resp.statAddr)
	}
}

func TestSpoofUnameDispatch(t *testing.T) {
	uts := &unix.Utsname{Sysname: [65]byte{}}
	copy(uts.Sysname[:], "runkube")
	copy(uts.Release[:], "6.6.0-spoofed")

	mux := NewMux()
	mux.Handle("UNAME", "*", SpoofUname(uts))
	config := mux.Build()

	resp := &fakeResponder{}
	ctx := &internal.Context{
		Syscall: unix.SYS_UNAME,
		Method:  "UNAME",
		Path:    "",
		Args:    [6]uint64{0xBEEF, 0, 0, 0, 0, 0},
		Respond: resp,
	}
	config.HandlerRegistry.Dispatch(ctx)

	if resp.action != "spoof_uname" {
		t.Errorf("expected spoof_uname, got %q", resp.action)
	}
	if resp.statAddr != 0xBEEF {
		t.Errorf("expected uname buf addr 0xBEEF, got %#x", resp.statAddr)
	}
	// No path required: handler must run even with an empty path
	ctx2 := &internal.Context{
		Syscall: unix.SYS_UNAME,
		Method:  "UNAME",
		Args:    [6]uint64{0xCAFE, 0, 0, 0, 0, 0},
		Respond: resp,
	}
	resp.action = ""
	config.HandlerRegistry.Dispatch(ctx2)
	if resp.action != "spoof_uname" {
		t.Errorf("expected spoof_uname without path, got %q", resp.action)
	}
}

func TestChainFirstResponderWins(t *testing.T) {
	first := internal.HandlerFunc(func(ctx *internal.Context) {})
	second := SpoofFile("second")

	chained := Chain(first, second)

	resp := &fakeResponder{}
	ctx := &internal.Context{
		Syscall: unix.SYS_OPENAT,
		Method:  "OPEN",
		Path:    "/x",
		Respond: resp,
	}
	chained.ServeSeccomp(ctx)

	// First handler did nothing (didn't respond), second should handle
	if resp.action != "spoof_fd" {
		t.Errorf("expected second handler to spoof fd, got %q", resp.action)
	}
}
