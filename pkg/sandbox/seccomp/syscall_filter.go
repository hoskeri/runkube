package seccomp

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

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

// SupervisorConfig contains dynamic handler callbacks for system calls.
type SupervisorConfig struct {
	// OpenCallback handles file open operations. Return (*content, nil) to spoof with an in-memory memfd,
	// or (nil, nil) to pass the call through natively to the kernel.
	OpenCallback func(pid uint32, path string) (*string, error)

	// StatCallback handles path-based fstatat queries. Return nil, nil to pass-through.
	StatCallback func(pid uint32, path string) (*unix.Stat_t, error)

	// StatxCallback handles modern path-based statx queries. Return nil, nil to pass-through.
	StatxCallback func(pid uint32, path string) (*unix.Statx_t, error)

	// StatfsCallback handles filesystem queries (e.g. statfs translations). Return nil, nil to pass-through.
	StatfsCallback func(pid uint32, path string) (*unix.Statfs_t, error)

	// ReadlinkCallback handles readlink and readlinkat queries. Return (*target, nil) to spoof, or (nil, nil) to pass-through.
	ReadlinkCallback func(pid uint32, path string) (*string, error)
}

type syscallMetadata struct {
	name       string
	pathArgIdx int // Index in Data.Args where the path string pointer lives
	handler    func(dupFd int, req *seccompNotif, path string, config *SupervisorConfig) (handled bool, err error)
}

var syscallRegistry = map[int32]syscallMetadata{
	unix.SYS_OPEN:       {name: "open", pathArgIdx: 0, handler: handleOpenFamily},
	unix.SYS_OPENAT:     {name: "openat", pathArgIdx: 1, handler: handleOpenFamily},
	unix.SYS_OPENAT2:    {name: "openat2", pathArgIdx: 1, handler: handleOpenFamily},
	unix.SYS_NEWFSTATAT: {name: "newfstatat", pathArgIdx: 1, handler: handleStatFamily},
	unix.SYS_STATX:      {name: "statx", pathArgIdx: 1, handler: handleStatxFamily},
	unix.SYS_STATFS:     {name: "statfs", pathArgIdx: 0, handler: handleStatfsFamily},
	unix.SYS_READLINK:   {name: "readlink", pathArgIdx: 0, handler: handleReadlinkFamily},
	unix.SYS_READLINKAT: {name: "readlinkat", pathArgIdx: 1, handler: handleReadlinkFamily},
}

// InstallFilter configures PR_SET_NO_NEW_PRIVS and loads the Seccomp filter
// targeting file open (open/openat/openat2), stat (newfstatat/statx), file system stat (statfs),
// and readlink (readlink/readlinkat) with SECCOMP_RET_USER_NOTIF.
func InstallFilter() (int, error) {
	slog.Debug("installing seccomp filter in child")

	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return -1, fmt.Errorf("failed to set PR_SET_NO_NEW_PRIVS: %w", err)
	}

	// Create Seccomp filter using raw SockFilter definitions
	filter := []unix.SockFilter{
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

// Supervise runs the parent supervisor loop. It blocks, listening for file operations
// on the listenerFd, reads memory via process_vm_readv(2), and delegates processing to
// highly focused specialized syscall handlers.
func Supervise(lfd *os.File, config SupervisorConfig) error {
	defer runtime.KeepAlive(lfd)
	defer lfd.Close()

	listenerFd := int(lfd.Fd())
	slog.Debug("supervisor loop started", "listener_fd", listenerFd)

	for {
		var req seccompNotif
		err := ioctl(listenerFd, unix.SECCOMP_IOCTL_NOTIF_RECV, unsafe.Pointer(&req))
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			// EBADF and ENOTCONN mean the seccomp notify FD itself is dead (child exited entirely).
			if errors.Is(err, unix.EBADF) || errors.Is(err, unix.ENOTCONN) {
				slog.Debug("target child exited, stopping supervisor loop", "error", err)
				return nil
			}
			// ENOENT and ESRCH here mean a pending system call was interrupted (e.g. via Go's SIGURG preemption)
			// exactly while the supervisor's RECV ioctl was dequeuing it. We must simply retry.
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
				slog.Debug("RECV ioctl interrupted by tracee signal preemption, retrying", "error", err)
				continue
			}
			return fmt.Errorf("RECV ioctl failed: %w", err)
		}

		// Resolve syscall metadata
		meta, registered := syscallRegistry[req.Data.Nr]
		if !registered {
			slog.Debug("unexpected syscall number in supervisor", "nr", req.Data.Nr)
			_ = continueNoop(listenerFd, req.Id)
			continue
		}

		// Resolve path from target child process space
		pathAddr := req.Data.Args[meta.pathArgIdx]
		path, err := readStringFromProcess(req.Pid, pathAddr)
		if err != nil {
			slog.Debug("failed to read path from child memory (process likely terminated)", "pid", req.Pid, "error", err)
			_ = continueNoop(listenerFd, req.Id)
			continue
		}

		cleanPath := filepath.Clean(path)
		slog.Debug("intercepted syscall in child", "pid", req.Pid, "syscall", meta.name, "path", cleanPath)

		// Dispatch to target handler
		handled, handlerErr := meta.handler(listenerFd, &req, cleanPath, &config)
		if handlerErr != nil {
			slog.Error("handler execution failed", "syscall", meta.name, "path", cleanPath, "error", handlerErr)
		}

		// Fall back to native execution if the handler declined or skipped spoofing
		if !handled {
			slog.Debug("no match found; executing passthrough", "path", cleanPath)
			err = continueNoop(listenerFd, req.Id)
			if err != nil && errors.Is(err, unix.ENOENT) {
				slog.Debug("continueNoop failed: notification expired or syscall interrupted", "id", req.Id)
			}
		}
	}
}

func handleOpenFamily(dupFd int, req *seccompNotif, path string, config *SupervisorConfig) (bool, error) {
	if config.OpenCallback == nil {
		return false, nil
	}

	mockContent, err := config.OpenCallback(req.Pid, path)
	if err != nil {
		return true, handleCallbackError(dupFd, req.Id, err)
	}
	if mockContent == nil {
		return false, nil
	}

	slog.Debug("path match found; injecting spoofed file descriptor", "path", path)
	memfd, err := unix.MemfdCreate("spoofed_asset", unix.MFD_CLOEXEC)
	if err != nil {
		_ = continueNoop(dupFd, req.Id)
		return true, fmt.Errorf("memfd_create failed: %w", err)
	}
	defer unix.Close(memfd)

	if _, err := unix.Write(memfd, []byte(*mockContent)); err != nil {
		_ = continueNoop(dupFd, req.Id)
		return true, fmt.Errorf("memfd write failed: %w", err)
	}

	if _, err := unix.Seek(memfd, 0, io.SeekStart); err != nil {
		_ = continueNoop(dupFd, req.Id)
		return true, fmt.Errorf("memfd seek failed: %w", err)
	}

	addfd := seccompNotifAddfd{
		Id:         req.Id,
		Flags:      unix.SECCOMP_ADDFD_FLAG_SEND,
		Srcfd:      uint32(memfd),
		Newfd:      0,
		NewfdFlags: 0,
	}

	err = ioctl(dupFd, unix.SECCOMP_IOCTL_NOTIF_ADDFD, unsafe.Pointer(&addfd))
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			slog.Debug("ADDFD failed: notification expired or syscall interrupted", "id", req.Id)
		} else {
			_ = continueNoop(dupFd, req.Id)
			return true, fmt.Errorf("ADDFD ioctl failed: %w", err)
		}
	}

	return true, nil
}

func handleStatFamily(dupFd int, req *seccompNotif, path string, config *SupervisorConfig) (bool, error) {
	if config.StatCallback == nil {
		return false, nil
	}

	stat, err := config.StatCallback(req.Pid, path)
	if err != nil {
		return true, handleCallbackError(dupFd, req.Id, err)
	}
	if stat == nil {
		return false, nil
	}

	statbufAddr := req.Data.Args[2]
	statBytes := (*[unsafe.Sizeof(*stat)]byte)(unsafe.Pointer(stat))[:]

	if writeErr := writeToProcess(req.Pid, statbufAddr, statBytes); writeErr != nil {
		_ = continueError(dupFd, req.Id, unix.EFAULT)
		return true, fmt.Errorf("failed to write stat structure to child: %w", writeErr)
	}

	return true, continueSuccess(dupFd, req.Id, 0)
}

func handleStatxFamily(dupFd int, req *seccompNotif, path string, config *SupervisorConfig) (bool, error) {
	if config.StatxCallback == nil {
		return false, nil
	}

	statx, err := config.StatxCallback(req.Pid, path)
	if err != nil {
		return true, handleCallbackError(dupFd, req.Id, err)
	}
	if statx == nil {
		return false, nil
	}

	statxbufAddr := req.Data.Args[4] // Arg[4] is *statxbuf
	statxBytes := (*[unsafe.Sizeof(*statx)]byte)(unsafe.Pointer(statx))[:]

	if writeErr := writeToProcess(req.Pid, statxbufAddr, statxBytes); writeErr != nil {
		_ = continueError(dupFd, req.Id, unix.EFAULT)
		return true, fmt.Errorf("failed to write statx structure to child: %w", writeErr)
	}

	return true, continueSuccess(dupFd, req.Id, 0)
}

func handleStatfsFamily(dupFd int, req *seccompNotif, path string, config *SupervisorConfig) (bool, error) {
	if config.StatfsCallback == nil {
		return false, nil
	}

	statfs, err := config.StatfsCallback(req.Pid, path)
	if err != nil {
		return true, handleCallbackError(dupFd, req.Id, err)
	}
	if statfs == nil {
		return false, nil
	}

	bufAddr := req.Data.Args[1] // Arg[1] is *buf (struct statfs)
	statfsBytes := (*[unsafe.Sizeof(*statfs)]byte)(unsafe.Pointer(statfs))[:]

	if writeErr := writeToProcess(req.Pid, bufAddr, statfsBytes); writeErr != nil {
		_ = continueError(dupFd, req.Id, unix.EFAULT)
		return true, fmt.Errorf("failed to write statfs structure to child: %w", writeErr)
	}

	return true, continueSuccess(dupFd, req.Id, 0)
}

func handleReadlinkFamily(dupFd int, req *seccompNotif, path string, config *SupervisorConfig) (bool, error) {
	if config.ReadlinkCallback == nil {
		return false, nil
	}

	target, err := config.ReadlinkCallback(req.Pid, path)
	if err != nil {
		return true, handleCallbackError(dupFd, req.Id, err)
	}
	if target == nil {
		return false, nil
	}

	bufAddr := req.Data.Args[1]
	bufSize := req.Data.Args[2]
	if req.Data.Nr == unix.SYS_READLINKAT {
		bufAddr = req.Data.Args[2]
		bufSize = req.Data.Args[3]
	}

	targetBytes := []byte(*target)
	if uint64(len(targetBytes)) > bufSize {
		targetBytes = targetBytes[:bufSize]
	}

	if len(targetBytes) > 0 {
		if writeErr := writeToProcess(req.Pid, bufAddr, targetBytes); writeErr != nil {
			_ = continueError(dupFd, req.Id, unix.EFAULT)
			return true, fmt.Errorf("failed to write readlink target to child: %w", writeErr)
		}
	}

	return true, continueSuccess(dupFd, req.Id, int64(len(targetBytes)))
}

func continueNoop(listenerFd int, reqId uint64) error {
	resp := seccompNotifResp{
		Id:    reqId,
		Val:   0,
		Error: 0,
		Flags: unix.SECCOMP_USER_NOTIF_FLAG_CONTINUE,
	}
	return ioctl(listenerFd, unix.SECCOMP_IOCTL_NOTIF_SEND, unsafe.Pointer(&resp))
}

func continueSuccess(listenerFd int, reqId uint64, val int64) error {
	resp := seccompNotifResp{
		Id:    reqId,
		Val:   val,
		Error: 0,
		Flags: 0,
	}
	return ioctl(listenerFd, unix.SECCOMP_IOCTL_NOTIF_SEND, unsafe.Pointer(&resp))
}

func continueError(listenerFd int, reqId uint64, errNo unix.Errno) error {
	resp := seccompNotifResp{
		Id:    reqId,
		Val:   -1,
		Error: int32(errNo),
		Flags: 0,
	}
	return ioctl(listenerFd, unix.SECCOMP_IOCTL_NOTIF_SEND, unsafe.Pointer(&resp))
}

func handleCallbackError(listenerFd int, reqId uint64, err error) error {
	if errno, ok := errors.AsType[unix.Errno](err); ok {
		return continueError(listenerFd, reqId, errno)
	}
	return continueError(listenerFd, reqId, unix.EIO)
}

// readStringFromProcess peeks into the target tracee's memory space via process_vm_readv(2).
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

		n, err := unix.ProcessVMReadv(int(pid), []unix.Iovec{localIov}, []unix.RemoteIovec{remoteIov}, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return "", err
		}
		if n <= 0 {
			break
		}

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

// writeToProcess writes data to the target tracee's memory space using process_vm_writev(2).
func writeToProcess(pid uint32, addr uint64, data []byte) error {
	written := 0
	for written < len(data) {
		localIov := unix.Iovec{
			Base: &data[written],
			Len:  uint64(len(data) - written),
		}
		remoteIov := unix.RemoteIovec{
			Base: uintptr(addr) + uintptr(written),
			Len:  len(data) - written,
		}

		n, err := unix.ProcessVMWritev(int(pid), []unix.Iovec{localIov}, []unix.RemoteIovec{remoteIov}, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if n <= 0 {
			return fmt.Errorf("unexpected short write of 0 bytes during process_vm_writev")
		}
		written += n
	}
	return nil
}

func ioctl(fd int, req uint, arg unsafe.Pointer) error {
	_, _, err := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg))
	if err != 0 {
		return err
	}
	return nil
}
