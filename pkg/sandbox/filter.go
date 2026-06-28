package sandbox

import (
	"fmt"
	"os"

	"github.com/hoskeri/runkube/pkg/sandbox/fdchan"
	"github.com/hoskeri/runkube/pkg/sandbox/seccomp"
)

func (s *Sandbox) setupSyscallFilter(responder func(fdchan.Message) error) error {
	if s.lastError != nil {
		return s.lastError
	}
	s.lastError = func() error {
		fd, err := seccomp.InstallFilter()
		if err != nil {
			return err
		}

		resp := fdchan.Message{Data: []byte("seccomp-filter-fd")}
		if err := resp.AddDescriptor(os.NewFile(uintptr(fd), "seccomp-filter-fd"), "seccomp-fd", "Seccomp user notify fd"); err != nil {
			return err
		}

		if err := responder(resp); err != nil {
			return err
		}

		return nil
	}()
	return s.lastError
}

func (s *Sandbox) startSyscallFilter(control *fdchan.Parent) error {

	procfsResp, err := control.Request(fdchan.Message{Data: []byte("seccomp-filter")})
	if err != nil {
		return err
	}
	if string(procfsResp.Data) != "seccomp-filter-fd" {
		return fmt.Errorf("seccomp-filter failed: unexpected response %s", string(procfsResp.Data))
	}

	seccompFilterFD, _, err := procfsResp.GetDescriptor("seccomp-fd")
	if err != nil {
		return fmt.Errorf("failed to get seccomp-fd descriptor: %w", err)
	}

	go func() {
		filterFD := seccompFilterFD
		if err := seccomp.Supervise(int(filterFD.Fd()), map[string]string{
			// "/proc/mounts": "fake-mount-data\n",
		}); err != nil {
			panic(err.Error())
		}
	}()
	return nil
}
