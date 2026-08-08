package sandbox

import (
	"fmt"
	"os"

	"github.com/hoskeri/runkube/pkg/sandbox/fdchan"
	"github.com/hoskeri/runkube/pkg/sandbox/seccomp"
)

// seccompFilterCmd is the fdchan command byte prefix for the seccomp filter handshake.
// The compiled BPF filter program bytes follow the command string.
const seccompFilterCmd = "seccomp-filter"

// setupSyscallFilter runs in the sandboxed child. It parses the BPF filter
// program sent by the parent, installs it on this process, and returns the
// seccomp user-notify listener FD to the parent.
func (s *Sandbox) setupSyscallFilter(req fdchan.Message, responder func(fdchan.Message) error) error {
	if s.lastError != nil {
		return s.lastError
	}
	s.lastError = func() error {
		// Parse filter program sent by the parent
		filterBytes := req.Data[len(seccompFilterCmd):]
		filter, err := seccomp.ParseFilter(filterBytes)
		if err != nil {
			return fmt.Errorf("failed to parse seccomp filter: %w", err)
		}

		fd, err := seccomp.InstallFilter(filter)
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

// startSyscallFilter runs in the parent. It builds the mux/filter from the
// sandbox config, sends the filter to the child for installation, and starts
// the supervisor loop once the child returns its listener FD.
func (s *Sandbox) startSyscallFilter(control *fdchan.Parent) error {
	filter, config := s.supervisorConfig()

	filterBytes, err := filter.MarshalBinary()
	if err != nil {
		return fmt.Errorf("failed to marshal seccomp filter: %w", err)
	}

	procfsResp, err := control.Request(fdchan.Message{
		Data: append([]byte(seccompFilterCmd), filterBytes...),
	})
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
		// prevent filterFD from getting garbage collected.
		if err := seccomp.Supervise(filterFD, config); err != nil {
			panic(err.Error())
		}
	}()
	return nil
}