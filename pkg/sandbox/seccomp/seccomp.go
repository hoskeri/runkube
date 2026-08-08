package seccomp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"github.com/hoskeri/runkube/pkg/sandbox/seccomp/supervisor"
	"golang.org/x/sys/unix"
)

var DefaultFilter = Filter{
	// Load Syscall Number (Offset 0 of seccomp_data struct)
	{Code: unix.BPF_LD + unix.BPF_W + unix.BPF_ABS, K: 0},
	// Check SYS_OPEN (jump 7 instructions on match)
	{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_OPEN), Jt: 7, Jf: 0},
	// Check SYS_OPENAT (jump 6 instructions on match)
	{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_OPENAT), Jt: 6, Jf: 0},
	// Check SYS_OPENAT2 (jump 5 instructions on match)
	{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_OPENAT2), Jt: 5, Jf: 0},
	// Check SYS_NEWFSTATAT (jump 4 instructions on match)
	{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_NEWFSTATAT), Jt: 4, Jf: 0},
	// Check SYS_STATX (jump 3 instructions on match)
	{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_STATX), Jt: 3, Jf: 0},
	// Check SYS_STATFS (jump 2 instructions on match)
	{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_STATFS), Jt: 2, Jf: 0},
	// Check SYS_READLINK (jump 1 instruction on match)
	{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_READLINK), Jt: 1, Jf: 0},
	// Check SYS_READLINKAT (match executes next, else jump to allow)
	{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_READLINKAT), Jt: 0, Jf: 1},
	// Match: Dispatch to Seccomp User Notifier listener
	{Code: unix.BPF_RET + unix.BPF_K, K: unix.SECCOMP_RET_USER_NOTIF},
	// No Match: Allow syscall execution
	{Code: unix.BPF_RET + unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
}

// InstallFilter configures PR_SET_NO_NEW_PRIVS and loads the Seccomp filter
// with SECCOMP_RET_USER_NOTIF.
func InstallFilter(f Filter) (int, error) {
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return -1, fmt.Errorf("failed to set PR_SET_NO_NEW_PRIVS: %w", err)
	}

	if len(f) == 0 {
		f = DefaultFilter
	}

	prog := unix.SockFprog{
		Len:    uint16(len(f)),
		Filter: &f[0],
	}

	// Install the filter with the listener creation flag.
	listenerFd, _, err := unix.Syscall(
		unix.SYS_SECCOMP,
		uintptr(unix.SECCOMP_SET_MODE_FILTER),
		uintptr(unix.SECCOMP_FILTER_FLAG_NEW_LISTENER),
		uintptr(unsafe.Pointer(&prog)),
	)

	if err != 0 {
		return -1, fmt.Errorf("failed to load seccomp filter: %w", err)
	}

	return int(listenerFd), nil
}

// Supervise runs the supervisor loop on the given listener FD.
// This is a convenience function that calls the internal supervisor.
func Supervise(lfd *os.File, config SupervisorConfig) error {
	return supervisor.Supervise(lfd, config)
}

// Filter is a compiled BPF filter program.
type Filter []unix.SockFilter

// sockFilterSize is the fixed wire size of a unix.SockFilter (no padding):
// Code(2) + Jt(1) + Jf(1) + K(4) = 8 bytes.
const sockFilterSize = 8

// MarshalBinary serializes the BPF filter program for transfer between
// processes (e.g. parent -> child over fdchan).
func (f Filter) MarshalBinary() ([]byte, error) {
	if len(f) == 0 {
		return nil, errors.New("cannot marshal empty filter")
	}
	buf := make([]byte, len(f)*sockFilterSize)
	for i, sf := range f {
		off := i * sockFilterSize
		binary.LittleEndian.PutUint16(buf[off:], sf.Code)
		buf[off+2] = sf.Jt
		buf[off+3] = sf.Jf
		binary.LittleEndian.PutUint32(buf[off+4:], sf.K)
	}
	return buf, nil
}

// ParseFilter deserializes a BPF filter program produced by MarshalBinary.
func ParseFilter(data []byte) (Filter, error) {
	if len(data) == 0 || len(data)%sockFilterSize != 0 {
		return nil, fmt.Errorf("invalid filter data length %d", len(data))
	}
	n := len(data) / sockFilterSize
	f := make(Filter, n)
	for i := range n {
		off := i * sockFilterSize
		f[i] = unix.SockFilter{
			Code: binary.LittleEndian.Uint16(data[off:]),
			Jt:   data[off+2],
			Jf:   data[off+3],
			K:    binary.LittleEndian.Uint32(data[off+4:]),
		}
	}
	return f, nil
}
