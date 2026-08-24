package sandbox

import (
	"os"

	"github.com/hoskeri/runkube/pkg/sandbox/seccomp"
)

// installSyscallFilter runs in the sandboxed child during the single setup
// RPC. It installs the seccomp user-notify filter on this process and
// returns the listener FD to hand to the parent in the setup response.
// The BPF program is the fixed DefaultFilter; custom programs are not
// supported.
func (s *Sandbox) installSyscallFilter() (int, error) {
	return seccomp.InstallFilter()
}

// superviseSyscallFilter runs in the parent. It builds the response mux from
// the sandbox config and starts the supervisor loop on the listener FD the
// child installed and returned in the setup response.
func (s *Sandbox) superviseSyscallFilter(lfd *os.File) error {
	config := s.supervisorConfig()

	go func() {
		// prevent lfd from getting garbage collected.
		if err := seccomp.Supervise(lfd, config); err != nil {
			panic(err.Error())
		}
	}()
	return nil
}
