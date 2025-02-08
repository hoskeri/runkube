package procfs

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Options configuration for the fake procfs
type Options struct {
	// RealProcPath is the path to the real proc filesystem (e.g. "/.real/proc")
	RealProcPath string

	// Overrides contains custom content handlers for specific files in procfs (relative to root, e.g. "cpuinfo")
	Overrides map[string]func(ctx context.Context) ([]byte, error)
}

// ProcNode represents a node in the fake procfs
type ProcNode struct {
	*fs.LoopbackNode
	fsOpts *Options
}

// Ensure ProcNode implements the NodeWrapChilder, NodeOpener, NodeGetattrer, and NodeReadlinker interfaces
var (
	_ fs.NodeWrapChilder = (*ProcNode)(nil)
	_ fs.NodeOpener      = (*ProcNode)(nil)
	_ fs.NodeGetattrer   = (*ProcNode)(nil)
	_ fs.NodeReadlinker  = (*ProcNode)(nil)
)

// WrapChild intercepts child node creation to ensure all children are also wrapped in ProcNode
func (n *ProcNode) WrapChild(ctx context.Context, ops fs.InodeEmbedder) fs.InodeEmbedder {
	loopbackNode, ok := ops.(*fs.LoopbackNode)
	if !ok {
		return ops
	}
	return &ProcNode{
		LoopbackNode: loopbackNode,
		fsOpts:       n.fsOpts,
	}
}

// Getattr overrides the default attributes for overridden files
func (n *ProcNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	// Populate with default attributes from the real procfs
	if errno := n.LoopbackNode.Getattr(ctx, fh, out); errno != 0 {
		return errno
	}

	// Adjust size if the file content is overridden
	path := n.Path(nil)
	if n.fsOpts != nil && n.fsOpts.Overrides != nil {
		if handler, ok := n.fsOpts.Overrides[path]; ok {
			content, err := handler(ctx)
			if err != nil {
				return syscall.EIO
			}
			out.Size = uint64(len(content))
		}
	}

	return 0
}

// Open overrides file open to serve custom content for overridden files
func (n *ProcNode) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	path := n.Path(nil)
	if n.fsOpts != nil && n.fsOpts.Overrides != nil {
		if handler, ok := n.fsOpts.Overrides[path]; ok {
			content, err := handler(ctx)
			if err != nil {
				return nil, 0, syscall.EIO
			}
			// Use FOPEN_DIRECT_IO to bypass kernel page cache so updates are immediate
			return &staticFileHandle{content: content}, fuse.FOPEN_DIRECT_IO, 0
		}
	}

	return n.LoopbackNode.Open(ctx, flags)
}

// Readlink overrides symlink reading for magic entries like "self" and "thread-self"
func (n *ProcNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	path := n.Path(nil)
	if path == "self" {
		caller, ok := fuse.FromContext(ctx)
		if !ok {
			return nil, syscall.EINVAL
		}
		return []byte(strconv.FormatUint(uint64(caller.Pid), 10)), 0
	}
	if path == "thread-self" {
		caller, ok := fuse.FromContext(ctx)
		if !ok {
			return nil, syscall.EINVAL
		}
		// thread-self points to self/task/<tid>
		return []byte(fmt.Sprintf("self/task/%d", caller.Pid)), 0
	}

	return n.LoopbackNode.Readlink(ctx)
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

// Mount mounts a new fake procfs instance at mountPoint delegating to realProcPath
func Mount(mountPoint string, opts *Options) (*fuse.Server, error) {
	if opts == nil || opts.RealProcPath == "" {
		return nil, fmt.Errorf("opts.RealProcPath must be set")
	}

	// Check if realProcPath exists and is a directory
	st, err := os.Stat(opts.RealProcPath)
	if err != nil {
		return nil, fmt.Errorf("stat real proc path %q: %w", opts.RealProcPath, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("real proc path %q is not a directory", opts.RealProcPath)
	}

	// Create root of loopback
	rootNode, err := fs.NewLoopbackRoot(opts.RealProcPath)
	if err != nil {
		return nil, fmt.Errorf("new loopback root: %w", err)
	}

	// Wrap root node in ProcNode
	procRoot := &ProcNode{
		LoopbackNode: rootNode.(*fs.LoopbackNode),
		fsOpts:       opts,
	}

	// Mount the server
	server, err := fs.Mount(mountPoint, procRoot, &fs.Options{
		MountOptions: fuse.MountOptions{
			Name: "procfs",
			// Keep procFS options typical for proc
			FsName: "proc",
		},
	})
	if err != nil {
		return nil, fmt.Errorf("fuse mount: %w", err)
	}

	return server, nil
}
