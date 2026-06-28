package procfs

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

func TestProcNodeOperationsDirect(t *testing.T) {
	// Create mock real proc directory
	tmpDir, err := os.MkdirTemp("", "procfs-unit-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "cpuinfo"), []byte("real cpuinfo\n"), 0644); err != nil {
		t.Fatalf("failed to write cpuinfo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "version"), []byte("real version\n"), 0644); err != nil {
		t.Fatalf("failed to write version: %v", err)
	}
	if err := os.Symlink("some-target", filepath.Join(tmpDir, "link")); err != nil {
		t.Fatalf("failed to create symlink: %v", err)
	}

	// Open the directory to get FD
	dirFile, err := os.Open(tmpDir)
	if err != nil {
		t.Fatalf("failed to open temp dir: %v", err)
	}
	realProcDirFD := int(dirFile.Fd())
	defer dirFile.Close()

	opts := &Options{
		Overrides: map[string]func(ctx context.Context) ([]byte, error){
			"cpuinfo": func(ctx context.Context) ([]byte, error) {
				return []byte("fake cpuinfo override\n"), nil
			},
		},
	}

	rootNode := &ProcNode{
		relPath:       "",
		realProcDirFD: realProcDirFD,
		fsOpts:        opts,
	}

	// Initialize the Inode hierarchy using NewNodeFS
	fs.NewNodeFS(rootNode, &fs.Options{})

	ctx := context.Background()

	// Look up nodes so they are properly initialized in the FUSE tree
	var entryOut fuse.EntryOut
	cpuInode, errno := rootNode.Lookup(ctx, "cpuinfo", &entryOut)
	if errno != 0 {
		t.Fatalf("Lookup cpuinfo failed: %v", errno)
	}
	cpuNode := cpuInode.Operations().(*ProcNode)

	verInode, errno := rootNode.Lookup(ctx, "version", &entryOut)
	if errno != 0 {
		t.Fatalf("Lookup version failed: %v", errno)
	}
	verNode := verInode.Operations().(*ProcNode)

	linkInode, errno := rootNode.Lookup(ctx, "link", &entryOut)
	if errno != 0 {
		t.Fatalf("Lookup link failed: %v", errno)
	}
	linkNode := linkInode.Operations().(*ProcNode)

	// Test Getattr on overridden file
	var attrOut fuse.AttrOut
	if errno := cpuNode.Getattr(ctx, nil, &attrOut); errno != 0 {
		t.Errorf("Getattr cpuinfo failed: %v", errno)
	}
	if attrOut.Attr.Size != uint64(len("fake cpuinfo override\n")) {
		t.Errorf("expected size %d, got %d", len("fake cpuinfo override\n"), attrOut.Attr.Size)
	}

	// Test Getattr on non-overridden file
	if errno := verNode.Getattr(ctx, nil, &attrOut); errno != 0 {
		t.Errorf("Getattr version failed: %v", errno)
	}
	if attrOut.Attr.Size != uint64(len("real version\n")) {
		t.Errorf("expected size %d, got %d", len("real version\n"), attrOut.Attr.Size)
	}

	// Test Readdir
	dirStream, errno := rootNode.Readdir(ctx)
	if errno != 0 {
		t.Errorf("Readdir failed: %v", errno)
	}
	var entries []fuse.DirEntry
	for dirStream.HasNext() {
		entry, errno := dirStream.Next()
		if errno != 0 {
			t.Fatalf("dirStream.Next failed: %v", errno)
		}
		entries = append(entries, entry)
	}
	foundCpuinfo := false
	foundVersion := false
	for _, entry := range entries {
		if entry.Name == "cpuinfo" {
			foundCpuinfo = true
		}
		if entry.Name == "version" {
			foundVersion = true
		}
	}
	if !foundCpuinfo || !foundVersion {
		t.Errorf("Readdir did not return expected files: %v", entries)
	}

	// Test Open & Read for overridden file
	fh, _, errno := cpuNode.Open(ctx, 0)
	if errno != 0 {
		t.Errorf("Open cpuinfo failed: %v", errno)
	}
	reader, ok := fh.(fs.FileReader)
	if !ok {
		t.Fatalf("expected FileReader")
	}
	dest := make([]byte, 100)
	res, errno := reader.Read(ctx, dest, 0)
	if errno != 0 {
		t.Errorf("Read cpuinfo failed: %v", errno)
	}
	buf := make([]byte, res.Size())
	data, status := res.Bytes(buf)
	if !status.Ok() {
		t.Errorf("res.Bytes failed: %v", status)
	}
	if string(data) != "fake cpuinfo override\n" {
		t.Errorf("expected fake cpuinfo, got %q", string(data))
	}

	// Test Open & Read for non-overridden file
	fhVer, _, errno := verNode.Open(ctx, 0)
	if errno != 0 {
		t.Errorf("Open version failed: %v", errno)
	}
	defer func() {
		if releaser, ok := fhVer.(fs.FileReleaser); ok {
			releaser.Release(ctx)
		}
	}()
	readerVer, ok := fhVer.(fs.FileReader)
	if !ok {
		t.Fatalf("expected FileReader for version")
	}
	resVer, errno := readerVer.Read(ctx, dest, 0)
	if errno != 0 {
		t.Errorf("Read version failed: %v", errno)
	}
	bufVer := make([]byte, resVer.Size())
	dataVer, statusVer := resVer.Bytes(bufVer)
	if !statusVer.Ok() {
		t.Errorf("res.Bytes for version failed: %v", statusVer)
	}
	if string(dataVer) != "real version\n" {
		t.Errorf("expected real version, got %q", string(dataVer))
	}

	// Test Readlink on real symlink
	target, errno := linkNode.Readlink(ctx)
	if errno != 0 {
		t.Errorf("Readlink link failed: %v", errno)
	}
	if string(target) != "some-target" {
		t.Errorf("expected 'some-target', got %q", string(target))
	}

	// Test Readlink on magic "self"
	// We can manually create a ProcNode here because we're just invoking Readlink which doesn't use the bridge
	selfNode := &ProcNode{
		relPath:       "self",
		realProcDirFD: realProcDirFD,
		fsOpts:        opts,
	}
	caller := &fuse.Caller{
		Owner: fuse.Owner{Uid: 1000, Gid: 1000},
		Pid:   999,
	}
	testCtx := fuse.NewContext(ctx, caller)
	targetSelf, errno := selfNode.Readlink(testCtx)
	if errno != 0 {
		t.Errorf("Readlink self failed: %v", errno)
	}
	if string(targetSelf) != "999" {
		t.Errorf("expected '999', got %q", string(targetSelf))
	}

	// Test Readlink on magic "thread-self"
	threadSelfNode := &ProcNode{
		relPath:       "thread-self",
		realProcDirFD: realProcDirFD,
		fsOpts:        opts,
	}
	targetThreadSelf, errno := threadSelfNode.Readlink(testCtx)
	if errno != 0 {
		t.Errorf("Readlink thread-self failed: %v", errno)
	}
	if string(targetThreadSelf) != "self/task/999" {
		t.Errorf("expected 'self/task/999', got %q", string(targetThreadSelf))
	}
}

func TestFDPassingHelpers(t *testing.T) {
	// Create socketpair
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("failed to create socketpair: %v", err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])

	// Create dummy files to represent fuseFD and realProcDirFD
	tmpFile1, err := os.CreateTemp("", "dummy-fuse-*")
	if err != nil {
		t.Fatalf("failed to create temp file 1: %v", err)
	}
	defer os.Remove(tmpFile1.Name())
	defer tmpFile1.Close()

	tmpFile2, err := os.CreateTemp("", "dummy-dir-*")
	if err != nil {
		t.Fatalf("failed to create temp file 2: %v", err)
	}
	defer os.Remove(tmpFile2.Name())
	defer tmpFile2.Close()

	// Spin up reader in background
	errChan := make(chan error, 1)
	fdChan := make(chan []int, 1)

	go func() {
		connFile := os.NewFile(uintptr(fds[0]), "parent-sock")
		defer connFile.Close()

		conn, err := net.FileConn(connFile)
		if err != nil {
			errChan <- err
			return
		}
		defer conn.Close()

		uc, ok := conn.(*net.UnixConn)
		if !ok {
			errChan <- fmt.Errorf("expected UnixConn")
			return
		}

		oob := make([]byte, unix.CmsgSpace(2*4))
		_, _, _, _, err = uc.ReadMsgUnix(nil, oob)
		if err != nil {
			errChan <- err
			return
		}

		msg, err := unix.ParseSocketControlMessage(oob)
		if err != nil {
			errChan <- err
			return
		}

		recvFDs, err := unix.ParseUnixRights(&msg[0])
		if err != nil {
			errChan <- err
			return
		}

		fdChan <- recvFDs
		errChan <- nil
	}()

	// Child writes the FDs
	connFile := os.NewFile(uintptr(fds[1]), "child-sock")
	conn, err := net.FileConn(connFile)
	if err != nil {
		t.Fatalf("child net.FileConn: %v", err)
	}
	uc := conn.(*net.UnixConn)
	oob := unix.UnixRights(int(tmpFile1.Fd()), int(tmpFile2.Fd()))
	_, _, err = uc.WriteMsgUnix(nil, oob, nil)
	if err != nil {
		t.Fatalf("child WriteMsgUnix: %v", err)
	}
	uc.Close()
	connFile.Close()

	// Wait for parent
	if err := <-errChan; err != nil {
		t.Fatalf("parent got error: %v", err)
	}

	recvFDs := <-fdChan
	if len(recvFDs) != 2 {
		t.Errorf("expected 2 FDs, got %d", len(recvFDs))
	} else {
		unix.Close(recvFDs[0])
		unix.Close(recvFDs[1])
	}
}
