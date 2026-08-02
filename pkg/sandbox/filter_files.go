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

func (s *Sandbox) supervisorConfig() seccomp.SupervisorConfig {
	return seccomp.SupervisorConfig{
		OpenCallback: func(_ uint32, p string) (*string, error) {
			switch p {
			case "/proc/mounts":
				return &staticMountData, nil
			case "/proc/swaps":
				return &staticSwaps, nil
			default:
				return nil, nil
			}
		},
	}
}
