package sandbox

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hoskeri/runkube/pkg/sandbox/fdchan"
	"github.com/hoskeri/runkube/pkg/sandbox/procfs"
	"golang.org/x/sys/unix"
)

func (s *Sandbox) startFakeProcFS(responder func(fdchan.Message) error) error {
	realProcPath := filepath.Join(s.rootDir(), ".real/proc")
	mountPoint := filepath.Join(s.rootDir(), "proc")

	if err := os.MkdirAll(mountPoint, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", realProcPath, err)
	}

	if err := os.MkdirAll(realProcPath, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", realProcPath, err)
	}

	if err := unix.Mount("proc", realProcPath, "proc", unix.MS_NODEV|unix.MS_NOEXEC|unix.MS_NOSUID|unix.MS_RELATIME, ""); err != nil {
		return fmt.Errorf("mount real procfs: %w", err)
	}

	fuseDevPath := filepath.Join("/dev/fuse")
	fuseFile, err := os.OpenFile(fuseDevPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %q: %w", fuseDevPath, err)
	}

	uid := os.Getuid()
	gid := os.Getgid()

	slog.Debug("opened fuse file descriptor", "dev", fuseFile.Fd())

	mountOpts := fmt.Sprintf("fd=%d,rootmode=040755,user_id=%d,group_id=%d", fuseFile.Fd(), uid, gid)
	mountErr := unix.Mount("procfs", mountPoint, "fuse", unix.MS_NOSUID|unix.MS_NODEV, mountOpts)
	if mountErr != nil {
		return fmt.Errorf("mount fuse: %w", mountErr)
	}

	realProcDir, err := os.Open(realProcPath)
	if err != nil {
		return fmt.Errorf("open real proc dir: %w", err)
	}
	defer realProcDir.Close()

	slog.Debug("device", "dev", fuseFile)

	resp := fdchan.Message{Data: []byte("mounted")}
	if err := resp.AddDescriptor(fuseFile, "fuse-conn", "FUSE connection file descriptor"); err != nil {
		return err
	}
	if err := resp.AddDescriptor(realProcDir, "real-proc-dir", "Real procfs directory file descriptor"); err != nil {
		return err
	}

	if err := responder(resp); err != nil {
		return err
	}
	return nil
}

func (s *Sandbox) startFakeProcServer(control *fdchan.Parent) error {
	procfsResp, err := control.Request(fdchan.Message{Data: []byte("mount-procfs")})
	if err != nil {
		return err
	}
	if string(procfsResp.Data) != "mounted" {
		return fmt.Errorf("procfs mount failed: unexpected response %s", string(procfsResp.Data))
	}

	fuseFile, _, err := procfsResp.GetDescriptor("fuse-conn")
	if err != nil {
		return fmt.Errorf("failed to get fuse-conn descriptor: %w", err)
	}
	realProcDir, _, err := procfsResp.GetDescriptor("real-proc-dir")
	if err != nil {
		return fmt.Errorf("failed to get real-proc-dir descriptor: %w", err)
	}

	server, err := procfs.ServeFD(int(fuseFile.Fd()), int(realProcDir.Fd()), &procfs.Options{})
	if err != nil {
		return fmt.Errorf("failed to start procfs FUSE server: %w", err)
	}

	go server.Wait()
	return nil
}
