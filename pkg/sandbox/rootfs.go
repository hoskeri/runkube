package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// Rootfs is the handle to the sandbox's root directory, opened by the child
// during setup and transferred to the parent in the setup response
// (docs/TODO.md, Sandbox → "Dynamic file injection").
//
// All methods take absolute container paths ("/sys/devices/system/node/node0")
// and resolve them with *at syscalls against the root directory fd, so
// injection never depends on the parent's mount namespace and keeps working
// after the child pivots into the rootfs.
//
// Writes are atomic (temp file + rename), which matters for two reasons:
//   - the child and parent concurrently materialize the same boot files, and
//     rename(2) needs only write permission on the parent directory, so two
//     writers can never race each other into EACCES on a read-only target;
//   - readers inside the container see either the old or the new content,
//     never a partially written file.
type Rootfs struct {
	f *os.File
}

// NewRootfs wraps a root directory file, taking ownership of it.
func NewRootfs(f *os.File) *Rootfs { return &Rootfs{f: f} }

// Close releases the root directory fd.
func (r *Rootfs) Close() error { return r.f.Close() }

// Write creates or replaces the file at the container path name with content,
// creating missing parent directories (mode 0755). The file is created with
// mode's permission bits (adjusted with fchmod, so the caller's umask cannot
// surprise it) and the content is written exactly; text callers include any
// trailing newline themselves.
func (r *Rootfs) Write(name string, content []byte, mode os.FileMode) error {
	rel, err := cleanRel(name)
	if err != nil {
		return err
	}
	if dir := path.Dir(rel); dir != "." {
		if err := r.MkdirAll(path.Join("/", dir), 0755); err != nil {
			return err
		}
	}
	// Create a uniquely-named sibling, write it fully, then rename over the
	// target: creation and update share one code path and neither ever opens
	// an existing target with O_WRONLY (which would EACCES on read-only
	// files like the system model's 0444 sysfs tree).
	tmp := tmpInjectName(rel)
	tfd, err := unix.Openat(int(r.f.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return &os.PathError{Op: "openat", Path: name, Err: err}
	}
	if err := unix.Fchmod(tfd, uint32(mode.Perm())); err != nil {
		unix.Close(tfd)
		unix.Unlinkat(int(r.f.Fd()), tmp, 0)
		return &os.PathError{Op: "fchmod", Path: name, Err: err}
	}
	if err := writeAll(tfd, content); err != nil {
		unix.Close(tfd)
		unix.Unlinkat(int(r.f.Fd()), tmp, 0)
		return &os.PathError{Op: "write", Path: name, Err: err}
	}
	if err := unix.Close(tfd); err != nil {
		unix.Unlinkat(int(r.f.Fd()), tmp, 0)
		return &os.PathError{Op: "close", Path: name, Err: err}
	}
	if err := unix.Renameat(int(r.f.Fd()), tmp, int(r.f.Fd()), rel); err != nil {
		unix.Unlinkat(int(r.f.Fd()), tmp, 0)
		return &os.PathError{Op: "renameat", Path: name, Err: err}
	}
	return nil
}

// MkdirAll creates the directory at the container path name and any missing
// parents, like os.MkdirAll but resolved against the root fd. Existing
// directories are left untouched.
func (r *Rootfs) MkdirAll(name string, mode uint32) error {
	rel, err := cleanRel(name)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	parts := strings.Split(rel, "/")
	for i := 1; i <= len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		if err := unix.Mkdirat(int(r.f.Fd()), prefix, mode); err != nil {
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			return &os.PathError{Op: "mkdirat", Path: "/" + prefix, Err: err}
		}
	}
	return nil
}

// cleanRel converts an absolute container path to the dirfd-relative form
// used by *at syscalls. "" and "/" map to the root itself. Absolute paths can
// never escape the root: path.Clean clamps leading ".." components at the
// top, and the ".." scan below is belt-and-braces.
func cleanRel(name string) (string, error) {
	if name == "" || name == "/" {
		return ".", nil
	}
	if !strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("rootfs path %q must be absolute", name)
	}
	clean := path.Clean(name)
	if clean == "/" {
		return ".", nil
	}
	for _, c := range strings.Split(clean[1:], "/") {
		if c == ".." {
			return "", fmt.Errorf("rootfs path %q escapes the root", name)
		}
	}
	return clean[1:], nil
}

// tmpInjectName returns a unique temporary sibling file name to build the
// final file next to, so the rename stays inside one directory (no cross-mount
// rename, and the target's permissions never gate the write).
func tmpInjectName(rel string) string {
	n := injectSeq.Add(1)
	if dir := path.Dir(rel); dir != "." {
		// Keep the sibling in the same directory as the target, so the
		// rename is not a cross-directory (or cross-mount) move.
		return dir + "/.rk-inject-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(n, 10)
	}
	return ".rk-inject-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(n, 10)
}

var injectSeq atomic.Uint64

// writeAll writes b to fd, retrying on EINTR. Regular files write the full
// buffer; anything short is an error.
func writeAll(fd int, b []byte) error {
	for len(b) > 0 {
		n, err := unix.Write(fd, b)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n <= 0 {
			return unix.EIO
		}
		b = b[n:]
	}
	return nil
}
