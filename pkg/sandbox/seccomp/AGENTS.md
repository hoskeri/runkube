# pkg/sandbox/seccomp

## Overview

This package implements a **seccomp-based system call interception framework**
using Linux's `SECCOMP_RET_USER_NOTIF` mechanism. It enables dynamic, runtime
interception and spoofing of filesystem-related system calls (open, stat,
statx, statfs, readlink) and identity calls (uname) from a privileged
supervisor process.

## Architecture

### Core Components

| File | Purpose |
|------|---------|
| `syscall_filter.go` | Main implementation: BPF filter installation, supervisor loop, syscall handlers |
| `syscall_filter_test.go` | End-to-end integration test using a child process with seccomp filter |

### Key Types

```go
// SupervisorConfig - Dynamic handler callbacks for intercepted syscalls
type SupervisorConfig struct {
    OpenCallback    func(pid uint32, path string) (*string, error)   // Spoof file content via memfd
    StatCallback    func(pid uint32, path string) (*unix.Stat_t, error)
    StatxCallback   func(pid uint32, path string) (*unix.Statx_t, error)
    StatfsCallback  func(pid uint32, path string) (*unix.Statfs_t, error)
    ReadlinkCallback func(pid uint32, path string) (*string, error)
}
```

### Intercepted Syscalls

| Syscall | Handler | Path Arg Index | Spoofing Mechanism |
|---------|---------|----------------|-------------------|
| `open` | `handleOpenFamily` | 0 | Inject memfd via `SECCOMP_IOCTL_NOTIF_ADDFD` |
| `openat` | `handleOpenFamily` | 1 | Inject memfd via `SECCOMP_IOCTL_NOTIF_ADDFD` |
| `openat2` | `handleOpenFamily` | 1 | Inject memfd via `SECCOMP_IOCTL_NOTIF_ADDFD` |
| `newfstatat` | `handleStatFamily` | 1 | Write `stat` struct to tracee memory via `process_vm_writev` |
| `statx` | `handleStatxFamily` | 1 | Write `statx` struct to tracee memory via `process_vm_writev` |
| `statfs` | `handleStatfsFamily` | 0 | Write `statfs` struct to tracee memory via `process_vm_writev` |
| `readlink` | `handleReadlinkFamily` | 0 | Write target string to tracee memory via `process_vm_writev` |
| `readlinkat` | `handleReadlinkFamily` | 1 | Write target string to tracee memory via `process_vm_writev` |
| `uname` | `SpoofUname` | -1 (no path; buf in arg 0) | Write `utsname` struct to tracee memory via `process_vm_writev` |

## Usage

### 1. Install Filter (in child/tracee process)

```go
import "golang.org/x/sys/unix"

func ChildProcess() {
    runtime.LockOSThread()

    // Set no new privs (required for seccomp)
    unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)

    // Install filter, returns listener FD
    listenerFd, err := seccomp.InstallFilter()
    if err != nil {
        log.Fatal(err)
    }

    // Pass listenerFd to supervisor via SCM_RIGHTS (unix domain socket)
    // ...
}
```

### 2. Run Supervisor (in parent/privileged process)

```go
func ParentProcess(listenerFd int) error {
    config := seccomp.SupervisorConfig{
        OpenCallback: func(pid uint32, path string) (*string, error) {
            if path == "/etc/secret" {
                content := "spoofed content"
                return &content, nil
            }
            return nil, nil // Pass through to kernel
        },
        StatCallback: func(pid uint32, path string) (*unix.Stat_t, error) {
            // Return custom stat or nil for passthrough
        },
        // ... other callbacks
    }

    return seccomp.Supervise(os.NewFile(uintptr(listenerFd), ""), config)
}
```

## BPF Filter Details

The filter (`InstallFilter`) always installs `DefaultFilter` and uses raw
`SockFilter` instructions:

```
1. Load syscall number (offset 0 of seccomp_data)
2. Compare against each target syscall (JEQ jumps)
3. On match: SECCOMP_RET_USER_NOTIF (dispatch to supervisor)
4. No match: SECCOMP_RET_ALLOW (execute normally)
```

Target syscalls: `open`, `openat`, `openat2`, `newfstatat`, `statx`, `statfs`, `readlink`, `readlinkat`, `uname`

## Supervisor Loop (`Supervise`)

1. **Receive notification** via `SECCOMP_IOCTL_NOTIF_RECV`
2. **Read path string** from tracee memory using `process_vm_readv`
3. **Dispatch** to registered handler based on syscall number
4. **Handler executes**:
   - Calls callback from `SupervisorConfig`
   - On spoof: writes result to tracee memory / injects FD
   - On passthrough: returns `false` → `continueNoop` (SECCOMP_USER_NOTIF_FLAG_CONTINUE)
5. **Reply** via `SECCOMP_IOCTL_NOTIF_SEND` or `SECCOMP_IOCTL_NOTIF_ADDFD`

## Memory Access Helpers

| Function | Purpose |
|----------|---------|
| `readStringFromProcess(pid, addr)` | Read NUL-terminated string via `process_vm_readv` |
| `writeToProcess(pid, addr, data)` | Write bytes via `process_vm_writev` |

## Error Handling

- **Callback returns error**: Converted to errno via `continueError` (default `EIO`)
- **Tracee exits**: `EBADF`/`ENOTCONN` on RECV → clean supervisor shutdown
- **Interrupted syscall**: `ENOENT`/`ESRCH` on RECV → retry loop
- **EINTR**: Retry both read/write and ioctl operations

## Testing

Run the integration test:

```bash
go test -v ./pkg/sandbox/seccomp/ -run TestSuperviseCallbacks
```

The test:
1. Creates socketpair for FD passing
2. Spawns child process (same test binary with `GO_WANT_SECCOMP_TEST_CHILD=1`)
3. Child installs filter, sends listener FD to parent via `SCM_RIGHTS`
4. Parent runs `Supervise` with callbacks
5. Child executes syscalls on mock paths (`/mock/open_file`, etc.)
6. Callbacks spoof responses; child validates results
7. Child exits → supervisor loop terminates via FD close

## Requirements

- Linux kernel ≥ 5.9 (for `SECCOMP_RET_USER_NOTIF` and `SECCOMP_IOCTL_NOTIF_ADDFD`)
- `golang.org/x/sys/unix` for raw syscalls and constants
- CAP_SYS_ADMIN or unprivileged user namespaces for seccomp operations

## Common Patterns

### Spoofing file content (open)

```go
OpenCallback: func(pid uint32, path string) (*string, error) {
    if strings.HasPrefix(path, "/virtual/") {
        content := generateVirtualFile(path)
        return &content, nil
    }
    return nil, nil // Native execution
}
```

### Virtual filesystem stats (stat/statx/statfs)

```go
StatCallback: func(pid uint32, path string) (*unix.Stat_t, error) {
    if path == "/virtual/device" {
        return &unix.Stat_t{Mode: unix.S_IFCHR | 0666, Rdev: 0x1234}, nil
    }
    return nil, nil
}
```

### Symlink redirection (readlink)

```go
ReadlinkCallback: func(pid uint32, path string) (*string, error) {
    if path == "/virtual/link" {
        target := "/real/target"
        return &target, nil
    }
    return nil, nil
}
```

## Thread Safety

- `InstallFilter` calls `runtime.LockOSThread()` — must run on dedicated thread
- `Supervise` blocks indefinitely — run in goroutine
- `SupervisorConfig` callbacks may be called concurrently for different syscalls
- Use `runtime.KeepAlive(lfd)` to prevent FD premature closure

## Integration Points

This package is used by the sandbox runtime to implement **virtual filesystem
layers**, allowing containers/sandboxes to see modified filesystem views
without actual filesystem changes (e.g., overlay configs, injected secrets,
virtual devices).
