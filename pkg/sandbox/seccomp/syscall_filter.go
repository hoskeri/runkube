package seccomp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// --- Private definitions for Seccomp User Notify (matching Linux UAPI) ---

type seccompData struct {
	Nr                 int32
	Arch               uint32
	InstructionPointer uint64
	Args               [6]uint64
}

type seccompNotif struct {
	Id    uint64
	Pid   uint32
	Flags uint32
	Data  seccompData
}

type seccompNotifResp struct {
	Id    uint64
	Val   int64
	Error int32
	Flags uint32
}

type seccompNotifAddfd struct {
	Id         uint64
	Flags      uint32
	Srcfd      uint32
	Newfd      uint32
	NewfdFlags uint32
}

// InstallFilter configures PR_SET_NO_NEW_PRIVS and loads the Seccomp filter
// targeting open (SYS_OPEN), openat (SYS_OPENAT), and openat2 (SYS_OPENAT2)
// with SECCOMP_RET_USER_NOTIF.
// It returns the seccomp listener file descriptor, which must be closed by the caller.
func InstallFilter() (int, error) {
	slog.Debug("installing seccomp filter in child", "syscalls", []string{"open", "openat", "openat2"})

	// Drop privileges to ensure non-root child can register seccomp filters.
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return -1, fmt.Errorf("failed to set PR_SET_NO_NEW_PRIVS: %w", err)
	}

	// Create Seccomp filter targeting open, openat, and openat2 using raw SockFilter definitions
	filter := []unix.SockFilter{
		// Load Syscall Number (Offset 0 of seccomp_data struct)
		{Code: unix.BPF_LD + unix.BPF_W + unix.BPF_ABS, K: 0},
		// Check if it matches openat (SYS_OPENAT). If True, jump 2 instructions forward to Match.
		{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_OPENAT), Jt: 2, Jf: 0},
		// Check if it matches open (SYS_OPEN). If True, jump 1 instruction forward to Match.
		{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_OPEN), Jt: 1, Jf: 0},
		// Check if it matches openat2 (SYS_OPENAT2). If True, execute next instruction. If False, skip next instruction.
		{Code: unix.BPF_JMP + unix.BPF_JEQ + unix.BPF_K, K: uint32(unix.SYS_OPENAT2), Jt: 0, Jf: 1},
		// Match (Instruction 4): Dispatch to Seccomp User Notifier listener
		{Code: unix.BPF_RET + unix.BPF_K, K: unix.SECCOMP_RET_USER_NOTIF},
		// No Match (Instruction 5): Allow syscall execution
		{Code: unix.BPF_RET + unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}

	prog := unix.SockFprog{
		Len:    uint16(len(filter)),
		Filter: &filter[0],
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

	slog.Debug("seccomp filter loaded successfully", "listener_fd", listenerFd)
	return int(listenerFd), nil
}

// Supervise runs the parent supervisor loop. It blocks, listening for file open
// notifications on the listenerFd, reads memory via process_vm_readv(2), and handles spoofing
// matching paths dynamically using atomic FD injection.
func Supervise(listenerFd int, mockFiles map[string]string) error {
	slog.Debug("supervisor loop started", "listener_fd", listenerFd)

	for {
		var req seccompNotif
		// SECCOMP_IOCTL_NOTIF_RECV blocks until a child makes a filtered syscall
		err := ioctl(listenerFd, unix.SECCOMP_IOCTL_NOTIF_RECV, unsafe.Pointer(&req))
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			if errors.Is(err, unix.EBADF) || errors.Is(err, unix.ENOTCONN) {
				slog.Debug("target child exited, stopping supervisor loop", "error", err)
				return nil
			}
			return fmt.Errorf("RECV ioctl failed: %w", err)
		}

		// Identify the path argument address based on the syscall triggered
		var pathAddr uint64
		var syscallName string

		switch req.Data.Nr {
		case int32(unix.SYS_OPENAT), int32(unix.SYS_OPENAT2):
			// For openat and openat2: openat(dfd, pathname, ...)
			pathAddr = req.Data.Args[1]
			if req.Data.Nr == int32(unix.SYS_OPENAT) {
				syscallName = "openat"
			} else {
				syscallName = "openat2"
			}
		case int32(unix.SYS_OPEN):
			// For open: open(pathname, flags, mode)
			pathAddr = req.Data.Args[0]
			syscallName = "open"
		default:
			slog.Debug("unexpected syscall number in supervisor", "nr", req.Data.Nr)
			continueNoop(listenerFd, req.Id)
			continue
		}

		path, err := readStringFromProcess(req.Pid, pathAddr)
		if err != nil {
			slog.Debug("failed to read path from child memory", "pid", req.Pid, "error", err)
			continueNoop(listenerFd, req.Id)
			continue
		}

		cleanPath := filepath.Clean(path)
		slog.Debug("intercepted syscall in child", "pid", req.Pid, "syscall", syscallName, "path", cleanPath)

		if mockContent, exists := mockFiles[cleanPath]; exists {
			slog.Debug("path match found; injecting spoofed file descriptor", "path", cleanPath)

			// Create an anonymous in-memory file
			memfd, err := unix.MemfdCreate("spoofed_asset", unix.MFD_CLOEXEC)
			if err != nil {
				slog.Debug("memfd_create failed", "error", err)
				continueNoop(listenerFd, req.Id)
				continue
			}

			// Write spoofed payload to memfd
			if _, err := unix.Write(memfd, []byte(mockContent)); err != nil {
				slog.Debug("memfd write failed", "error", err)
				unix.Close(memfd)
				continueNoop(listenerFd, req.Id)
				continue
			}

			// Rewind fd so the child reads it from byte 0
			if _, err := unix.Seek(memfd, 0, io.SeekStart); err != nil {
				slog.Debug("memfd seek failed", "error", err)
				unix.Close(memfd)
				continueNoop(listenerFd, req.Id)
				continue
			}

			// Atomically inject the memfd into target and respond to notification
			addfd := seccompNotifAddfd{
				Id:         req.Id,
				Flags:      unix.SECCOMP_ADDFD_FLAG_SEND, // Atomically returns the FD as syscall return val
				Srcfd:      uint32(memfd),
				Newfd:      0, // Let OS pick child's fd slot
				NewfdFlags: 0,
			}

			err = ioctl(listenerFd, unix.SECCOMP_IOCTL_NOTIF_ADDFD, unsafe.Pointer(&addfd))
			unix.Close(memfd) // Safe to close local end now
			if err != nil {
				slog.Debug("ADDFD ioctl failed (requires Linux >= 5.14)", "error", err)
				continueNoop(listenerFd, req.Id)
			}
		} else {
			slog.Debug("no match found; executing passthrough", "path", cleanPath)
			continueNoop(listenerFd, req.Id)
		}
	}
}

// Tells the kernel to let the system call execute normally.
func continueNoop(listenerFd int, reqId uint64) {
	resp := seccompNotifResp{
		Id:    reqId,
		Val:   0,
		Error: 0,
		Flags: unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE,
	}
	_ = ioctl(listenerFd, unix.SECCOMP_IOCTL_NOTIF_SEND, unsafe.Pointer(&resp))
}

// Low-level helper to peek into the target tracee's memory space using process_vm_readv(2)
func readStringFromProcess(pid uint32, addr uint64) (string, error) {
	var result []byte
	offset := addr
	buf := make([]byte, 256)

	for {
		localIov := unix.Iovec{
			Base: &buf[0],
			Len:  uint64(len(buf)),
		}
		remoteIov := unix.RemoteIovec{
			Base: uintptr(offset),
			Len:  len(buf),
		}

		// Perform zero-copy process virtual memory read across address spaces
		n, err := unix.ProcessVMReadv(int(pid), []unix.Iovec{localIov}, []unix.RemoteIovec{remoteIov}, 0)
		if err != nil {
			return "", err
		}
		if n <= 0 {
			break
		}

		// Look for the NULL terminator in the chunk
		for i := range n {
			if buf[i] == 0 {
				result = append(result, buf[:i]...)
				return string(result), nil
			}
		}
		result = append(result, buf[:n]...)
		offset += uint64(n)
	}
	return string(result), nil
}

func ioctl(fd int, req uint, arg unsafe.Pointer) error {
	_, _, err := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg))
	if err != 0 {
		return err
	}
	return nil
}
