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

func (s *Sandbox) supervisorConfig() seccomp.SupervisorConfig {
	sys := s.system()

	mux := seccomp.NewMux()

	// Static mount table
	mux.Handle("OPEN", "/proc/mounts", seccomp.SpoofFile(staticMountData))

	// Emulated hardware & resources (typed System model)
	mux.Handle("OPEN", "/proc/cpuinfo", seccomp.SpoofFile(sys.CpuInfo()))
	mux.Handle("OPEN", "/proc/meminfo", seccomp.SpoofFile(sys.MemInfo()))
	mux.Handle("OPEN", "/proc/swaps", seccomp.SpoofFile(sys.Swaps()))
	mux.Handle("OPEN", "/proc/uptime", seccomp.SpoofFile(sys.Uptime()))
	mux.Handle("OPEN", "/proc/loadavg", seccomp.SpoofFile(sys.LoadAvg()))
	mux.Handle("OPEN", "/proc/stat", seccomp.SpoofFile(sys.ProcStat()))

	// Kernel identity: boot id, version, hostname
	mux.Handle("OPEN", "/proc/version", seccomp.SpoofFile(sys.ProcVersion()))
	mux.Handle("OPEN", "/proc/sys/kernel/boot_id", seccomp.SpoofFile(sys.BootID()))
	mux.Handle("OPEN", "/proc/sys/kernel/random/boot_id", seccomp.SpoofFile(sys.BootID()))
	mux.Handle("OPEN", "/proc/sys/kernel/osrelease", seccomp.SpoofFile(sys.KernelRelease()))
	mux.Handle("OPEN", "/proc/sys/kernel/version", seccomp.SpoofFile(sys.KernelVersion()))
	mux.Handle("OPEN", "/proc/sys/kernel/hostname", seccomp.SpoofFile(s.hostname()+"\n"))
	mux.Handle("OPEN", "/proc/sys/kernel/domainname", seccomp.SpoofFile(sys.DomainName()))

	// uname(2)
	mux.Handle("UNAME", "*", seccomp.SpoofUname(sys.Utsname(s.hostname())))

	// Catch-all passthrough (optional - implicit if no match)
	mux.Handle("OPEN", "*", seccomp.PassThrough())

	// Middleware
	mux.Use(seccomp.LoggingMiddleware())

	return mux.Build()
}
