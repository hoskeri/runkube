package supervisor

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/hoskeri/runkube/pkg/sandbox/seccomp/internal"
)

// seccompData matches the kernel's seccomp_data structure
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

// Supervise runs the parent supervisor loop.
// It blocks, listening for file operations on the listenerFd,
// reads memory via process_vm_readv(2), and delegates processing to handlers.
func Supervise(lfd *os.File, config internal.SupervisorConfig) error {
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
			// ENOENT and ESRCH mean a pending system call was interrupted (e.g. via Go's SIGURG preemption)
			// exactly while the supervisor's RECV ioctl was dequeuing it. We must simply retry.
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
				slog.Debug("RECV ioctl interrupted by tracee signal preemption, retrying", "error", err)
				continue
			}
			return fmt.Errorf("RECV ioctl failed: %w", err)
		}

		// Resolve syscall metadata
		meta := config.SyscallRegistry.Lookup(req.Data.Nr)
		if meta == nil {
			slog.Debug("unexpected syscall number in supervisor", "nr", req.Data.Nr)
			_ = continueNoop(listenerFd, req.Id)
			continue
		}

		// Resolve path from target child process space (if the syscall has one).
		// Some intercepted syscalls (e.g. uname) take no path argument.
		cleanPath := ""
		if meta.PathArgIdx >= 0 {
			pathAddr := req.Data.Args[meta.PathArgIdx]
			path, err := readStringFromProcess(req.Pid, pathAddr)
			if err != nil {
				slog.Debug("failed to read path from child memory (process likely terminated)", "pid", req.Pid, "error", err)
				_ = continueNoop(listenerFd, req.Id)
				continue
			}
			cleanPath = filepath.Clean(path)
		}
		slog.Debug("intercepted syscall in child", "pid", req.Pid, "syscall", meta.Name, "method", meta.Method, "path", cleanPath)

		// Build context
		flagsArg := max(meta.FlagsArgIdx, 0)
		maskArg := max(meta.MaskArgIdx, 0)
		ctx := &internal.Context{
			PID:     req.Pid,
			Syscall: req.Data.Nr,
			Method:  meta.Method,
			Path:    cleanPath,
			Args:    req.Data.Args,
			DirFD:   resolveDirFD(req.Data.Args, meta),
			Flags:   req.Data.Args[flagsArg],
			Mask:    uint32(req.Data.Args[maskArg]),
			Respond: &responder{
				lfd: listenerFd,
				id:  req.Id,
				pid: req.Pid,
			},
		}

		// Dispatch to handler registry
		config.HandlerRegistry.Dispatch(ctx)
	}
}

// responder implements internal.Responder
type responder struct {
	lfd int
	id  uint64
	pid uint32
}

func (r *responder) Continue() error {
	return continueNoop(r.lfd, r.id)
}

func (r *responder) Error(errno unix.Errno) error {
	return continueError(r.lfd, r.id, errno)
}

func (r *responder) SpoofFD(fd int) error {
	addfd := seccompNotifAddfd{
		Id:         r.id,
		Flags:      unix.SECCOMP_ADDFD_FLAG_SEND,
		Srcfd:      uint32(fd),
		Newfd:      0,
		NewfdFlags: 0,
	}
	err := ioctl(r.lfd, unix.SECCOMP_IOCTL_NOTIF_ADDFD, unsafe.Pointer(&addfd))
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			slog.Debug("ADDFD failed: notification expired or syscall interrupted", "id", r.id)
		} else {
			_ = continueNoop(r.lfd, r.id)
			return fmt.Errorf("ADDFD ioctl failed: %w", err)
		}
	}
	return nil
}

func (r *responder) SpoofStat(addr uint64, stat any) error {
	// Handle the underlying struct type generically using reflection
	statBytes, err := structToBytes(stat)
	if err != nil {
		_ = continueError(r.lfd, r.id, unix.EFAULT)
		return err
	}

	if writeErr := writeToProcess(r.pid, addr, statBytes); writeErr != nil {
		_ = continueError(r.lfd, r.id, unix.EFAULT)
		return fmt.Errorf("failed to write stat structure to child: %w", writeErr)
	}
	return continueSuccess(r.lfd, r.id, 0)
}

// structToBytes converts a struct to its raw byte representation using reflection
func structToBytes(v any) ([]byte, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer {
		return nil, fmt.Errorf("stat must be a pointer to struct, got %T", v)
	}
	rv = rv.Elem()
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("stat must be a pointer to struct, got %T", v)
	}

	size := rv.Type().Size()
	buf := make([]byte, size)

	// Use unsafe to copy the struct memory
	src := unsafe.Slice((*byte)(unsafe.Pointer(rv.UnsafeAddr())), size)
	copy(buf, src)
	return buf, nil
}

func (r *responder) SpoofReadlink(addr uint64, target string, bufSize uint64) error {
	targetBytes := []byte(target)
	if uint64(len(targetBytes)) > bufSize {
		targetBytes = targetBytes[:bufSize]
	}
	if len(targetBytes) > 0 {
		if writeErr := writeToProcess(r.pid, addr, targetBytes); writeErr != nil {
			_ = continueError(r.lfd, r.id, unix.EFAULT)
			return fmt.Errorf("failed to write readlink target to child: %w", writeErr)
		}
	}
	return continueSuccess(r.lfd, r.id, int64(len(targetBytes)))
}

func (r *responder) SpoofUname(addr uint64, uts *unix.Utsname) error {
	utsBytes, err := structToBytes(uts)
	if err != nil {
		_ = continueError(r.lfd, r.id, unix.EFAULT)
		return err
	}
	if writeErr := writeToProcess(r.pid, addr, utsBytes); writeErr != nil {
		_ = continueError(r.lfd, r.id, unix.EFAULT)
		return fmt.Errorf("failed to write utsname to child: %w", writeErr)
	}
	return continueSuccess(r.lfd, r.id, 0)
}

func (r *responder) Success(ret int64) error {
	return continueSuccess(r.lfd, r.id, ret)
}

// --- Low-level helpers ---

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

func resolveDirFD(args [6]uint64, meta *internal.SyscallMeta) int {
	if meta.DirFDArgIdx >= 0 {
		return int(args[meta.DirFDArgIdx])
	}
	return unix.AT_FDCWD
}

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
