package procfs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

// Options configuration for the fake procfs
type Options struct {
	// Overrides contains custom content handlers for specific files in procfs (relative to root, e.g. "cpuinfo")
	Overrides map[string]func(ctx context.Context) ([]byte, error)
}

// ProcNode represents a node in the fake procfs using direct relative descriptor operations
type ProcNode struct {
	fs.Inode
	relPath       string
	realProcDirFD int
	fsOpts        *Options
}

// Ensure ProcNode implements the NodeLookuper, NodeOpener, NodeGetattrer, NodeReadlinker, and NodeReaddirer interfaces
var (
	_ fs.NodeLookuper   = (*ProcNode)(nil)
	_ fs.NodeOpener     = (*ProcNode)(nil)
	_ fs.NodeGetattrer  = (*ProcNode)(nil)
	_ fs.NodeReadlinker = (*ProcNode)(nil)
	_ fs.NodeReaddirer  = (*ProcNode)(nil)
)

func toErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	if errno, ok := err.(syscall.Errno); ok {
		return errno
	}
	return syscall.EIO
}

func toSyscallStat(s *unix.Stat_t) *syscall.Stat_t {
	slog.Info("s", "stat", s)
	return &syscall.Stat_t{
		Dev:     s.Dev,
		Ino:     s.Ino,
		Nlink:   s.Nlink,
		Mode:    s.Mode,
		Uid:     0,
		Gid:     0,
		Rdev:    s.Rdev,
		Size:    4096,
		Blksize: s.Blksize,
		Blocks:  s.Blocks,
		Atim:    syscall.Timespec{Sec: s.Atim.Sec, Nsec: s.Atim.Nsec},
		Mtim:    syscall.Timespec{Sec: s.Mtim.Sec, Nsec: s.Mtim.Nsec},
		Ctim:    syscall.Timespec{Sec: s.Ctim.Sec, Nsec: s.Ctim.Nsec},
	}
}

func (n *ProcNode) getRelPath() string {
	if n.relPath == "" {
		return "."
	}
	return n.relPath
}

// Getattr overrides the default attributes for overridden files
func (n *ProcNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	// Adjust size if the file content is overridden
	if n.fsOpts != nil && n.fsOpts.Overrides != nil {
		if handler, ok := n.fsOpts.Overrides[n.relPath]; ok {
			content, err := handler(ctx)
			if err != nil {
				return syscall.EIO
			}
			out.Attr.Mode = syscall.S_IFREG | 0444
			out.Attr.Size = uint64(len(content))
			return 0
		}
	}

	var stat unix.Stat_t
	err := unix.Fstatat(n.realProcDirFD, n.getRelPath(), &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return toErrno(err)
	}

	out.Attr.FromStat(toSyscallStat(&stat))
	return 0
}

// Lookup finds a child node by name relative to the realProcDirFD
func (n *ProcNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childRelPath := name
	if n.relPath != "" {
		childRelPath = filepath.Join(n.relPath, name)
	}

	var stat unix.Stat_t
	err := unix.Fstatat(n.realProcDirFD, childRelPath, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return nil, toErrno(err)
	}

	out.Attr.FromStat(toSyscallStat(&stat))

	stable := fs.StableAttr{
		Mode: uint32(stat.Mode),
		Ino:  stat.Ino,
		Gen:  1,
	}

	childNode := &ProcNode{
		relPath:       childRelPath,
		realProcDirFD: n.realProcDirFD,
		fsOpts:        n.fsOpts,
	}

	return n.NewInode(ctx, childNode, stable), 0
}

// Open overrides file open to serve custom content for overridden files or read from real files
func (n *ProcNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	if n.fsOpts != nil && n.fsOpts.Overrides != nil {
		if handler, ok := n.fsOpts.Overrides[n.relPath]; ok {
			content, err := handler(ctx)
			if err != nil {
				return nil, 0, syscall.EIO
			}
			return &staticFileHandle{content: content}, fuse.FOPEN_DIRECT_IO, 0
		}
	}

	fileFD, err := unix.Openat(n.realProcDirFD, n.getRelPath(), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, 0, toErrno(err)
	}

	return &realFileHandle{fd: fileFD}, fuse.FOPEN_DIRECT_IO, 0
}

// Readlink overrides symlink reading for magic entries like "self" and "thread-self"
func (n *ProcNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	if n.relPath == "self" {
		caller, ok := fuse.FromContext(ctx)
		if !ok {
			return nil, syscall.EINVAL
		}
		return []byte(strconv.FormatUint(uint64(caller.Pid), 10)), 0
	}
	if n.relPath == "thread-self" {
		caller, ok := fuse.FromContext(ctx)
		if !ok {
			return nil, syscall.EINVAL
		}
		return fmt.Appendf(nil, "self/task/%d", caller.Pid), 0
	}

	buf := make([]byte, 1024)
	for {
		nBytes, err := unix.Readlinkat(n.realProcDirFD, n.getRelPath(), buf)
		if err != nil {
			return nil, toErrno(err)
		}
		if nBytes < len(buf) {
			return buf[:nBytes], 0
		}
		buf = make([]byte, len(buf)*2)
	}
}

// Readdir reads directory entries relative to realProcDirFD
func (n *ProcNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	dirFD, err := unix.Openat(n.realProcDirFD, n.getRelPath(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, toErrno(err)
	}
	f := os.NewFile(uintptr(dirFD), "")
	defer f.Close()

	infos, err := f.Readdir(-1)
	if err != nil {
		return nil, toErrno(err)
	}

	var entries []fuse.DirEntry
	for _, info := range infos {
		var ino uint64
		var mode uint32
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			ino = stat.Ino
			mode = uint32(stat.Mode)
		} else {
			// S_IFREG fallback
			mode = uint32(syscall.S_IFREG)
			if info.IsDir() {
				mode = uint32(syscall.S_IFDIR)
			}
		}
		entries = append(entries, fuse.DirEntry{
			Name: info.Name(),
			Mode: mode,
			Ino:  ino,
		})
	}

	return fs.NewListDirStream(entries), 0
}

// staticFileHandle serves static/dynamic pre-rendered content for overridden files
type staticFileHandle struct {
	content []byte
}

var _ fs.FileReader = (*staticFileHandle)(nil)

func (fh *staticFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off >= int64(len(fh.content)) {
		return fuse.ReadResultData(nil), 0
	}
	end := min(off+int64(len(dest)), int64(len(fh.content)))
	return fuse.ReadResultData(fh.content[off:end]), 0
}

// realFileHandle wraps a descriptor to a file on the real procfs
type realFileHandle struct {
	fd int
}

var (
	_ fs.FileReader   = (*realFileHandle)(nil)
	_ fs.FileReleaser = (*realFileHandle)(nil)
)

func (fh *realFileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {

	n, err := unix.Pread(fh.fd, dest, off)
	if err != nil {
		return nil, toErrno(err)
	}
	slog.Info("read", "fd", fh.fd, "data", dest)
	return fuse.ReadResultData(dest[:n]), 0
}

func (fh *realFileHandle) Release(ctx context.Context) syscall.Errno {
	unix.Close(fh.fd)
	return 0
}

// ServeFD starts a FUSE server on the given fuseFD (pre-mounted connection) and proxies to realProcDirFD.
func ServeFD(fuseFD int, realProcDirFD int, opts *Options) (*fuse.Server, error) {
	rootNode := &ProcNode{
		relPath:       "",
		realProcDirFD: realProcDirFD,
		fsOpts:        opts,
	}

	server, err := fs.Mount(fmt.Sprintf("/dev/fd/%d", fuseFD), rootNode, &fs.Options{
		MountOptions: fuse.MountOptions{
			Name:       "procfs",
			FsName:     "proc",
			AllowOther: true,
			Debug:      true,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("fuse mount on FD %d: %w", fuseFD, err)
	}

	return server, nil
}
