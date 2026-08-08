package sandbox

import (
	"github.com/hoskeri/runkube/pkg/sandbox/seccomp"
)

var staticMountData = `
/dev/root / ext4 rw,relatime,errors=remount-ro 0 0
sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
udev /dev devtmpfs rw,nosuid,relatime 0 0
tmpfs /dev/shm tmpfs rw,nosuid,nodev,inode64 0 0
cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec 0 0
`
var staticSwaps = `
Filename				Type		Size		Used		Priority
`

func (s *Sandbox) supervisorConfig() (seccomp.Filter, seccomp.SupervisorConfig) {
	mux := seccomp.NewMux()

	// Static file spoofing
	mux.Handle("OPEN", "/proc/mounts", seccomp.SpoofFile(staticMountData))
	mux.Handle("OPEN", "/proc/swaps", seccomp.SpoofFile(staticSwaps))

	// Catch-all passthrough (optional - implicit if no match)
	mux.Handle("OPEN", "*", seccomp.PassThrough())

	// Middleware
	mux.Use(seccomp.LoggingMiddleware())

	return mux.Build()
}