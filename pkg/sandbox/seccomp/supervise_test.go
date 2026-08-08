package seccomp

import (
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	envVarTestChild = "GO_WANT_SECCOMP_TEST_CHILD"

	mockOpenPath     = "/mock/open_file"
	mockStatPath     = "/mock/stat_file"
	mockStatxPath    = "/mock/statx_file"
	mockStatfsPath   = "/mock/statfs_file"
	mockReadlinkPath = "/mock/readlink_file"

	mockOpenContent    = "dynamic seccomp spoofed payload!"
	mockStatIno        = 12345
	mockStatxIno       = 54321
	mockStatfsType     = 0x1337BEEF
	mockReadlinkTarget = "/mock/symlink_target"
)

// buildTestMux constructs the mux with all mock routes for the end-to-end test
func buildTestMux() (Filter, SupervisorConfig) {
	mux := NewMux()

	mux.Handle("OPEN", mockOpenPath, SpoofFile(mockOpenContent))
	mux.Handle("STAT", mockStatPath, SpoofStat(&unix.Stat_t{
		Ino:  mockStatIno,
		Mode: unix.S_IFREG | 0644,
		Size: int64(len(mockOpenContent)),
	}))
	mux.Handle("STATX", mockStatxPath, SpoofStat(&unix.Statx_t{
		Mask: unix.STATX_BASIC_STATS,
		Ino:  mockStatxIno,
		Mode: unix.S_IFREG | 0755,
	}))
	mux.Handle("STATFS", mockStatfsPath, SpoofStat(&unix.Statfs_t{
		Type: mockStatfsType,
	}))
	mux.Handle("READLINK", mockReadlinkPath, SpoofReadlink(mockReadlinkTarget))

	return mux.Build()
}

// TestSuperviseCallbacks validates the end-to-end interception and spoofing of all
// registered system calls through the Mux-based API.
func TestSuperviseCallbacks(t *testing.T) {
	if os.Getenv(envVarTestChild) == "1" {
		runTestChild(t)
		return
	}

	// 1. Setup socket synchronization for passing the seccomp listener FD
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Failed to create Socketpair: %v", err)
	}
	parentSock := fds[0]
	childSock := fds[1]

	// 2. Build the filter + config
	filter, config := buildTestMux()
	filterBytes, err := filter.MarshalBinary()
	if err != nil {
		t.Fatalf("Failed to marshal filter: %v", err)
	}

	// 3. Spawn the child process targeting this same test runner
	cmd := exec.Command(os.Args[0], "-test.v", "-test.run=TestSuperviseCallbacks")
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=1", envVarTestChild))
	// Pass the filter bytes through the environment (base64) - the child installs them
	cmd.Env = append(cmd.Env, testChildFilterEnv+"="+base64.StdEncoding.EncodeToString(filterBytes))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	childFile := os.NewFile(uintptr(childSock), "child_sock")
	cmd.ExtraFiles = []*os.File{childFile}

	if err := cmd.Start(); err != nil {
		childFile.Close()
		unix.Close(parentSock)
		t.Fatalf("Failed to start child process: %v", err)
	}
	childFile.Close()

	// Retrieve the seccomp notify FD from the child
	listenerFd, err := recvFD(parentSock)
	if err != nil {
		_ = cmd.Process.Kill()
		unix.Close(parentSock)
		t.Fatalf("Failed to receive seccomp listener FD: %v", err)
	}
	defer unix.Close(listenerFd)

	// 4. Run the supervisor loop in the background using the config
	errChan := make(chan error, 1)
	go func() {
		errChan <- Supervise(os.NewFile(uintptr(listenerFd), ""), config)
	}()

	// Signal the child that our supervisor loop is active and waiting
	if _, err := unix.Write(parentSock, []byte{1}); err != nil {
		t.Fatalf("Failed to write synchronization byte to child: %v", err)
	}
	_ = unix.Close(parentSock)

	// 5. Wait for the child to run all its assertions and exit
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Child process returned with an error: %v", err)
	}

	// Stop supervisor loop by closing the listener FD
	_ = unix.Close(listenerFd)
	if superviseErr := <-errChan; superviseErr != nil {
		t.Logf("Supervisor exited: %v", superviseErr)
	}
}

// runTestChild is executed by the spawned child process to run individual system call assertions.
func runTestChild(t *testing.T) {
	parentSock := 3 // Inherited socketpair FD

	// Re-parse the filter from the environment
	encFilter, err := base64.StdEncoding.DecodeString(os.Getenv(testChildFilterEnv))
	if err != nil {
		slog.Error("Child failed to decode filter env", "error", err)
		os.Exit(1)
	}

	_, err = ParseFilter(encFilter)

	if err != nil {
		slog.Error("Child failed to parse filter", "error", err)
		os.Exit(1)
	}

	// 1. Install Seccomp notify filter on this single thread
	listenerFd, err := InstallFilter(nil)
	if err != nil {
		slog.Error("Child failed to install seccomp filter", "error", err)
		os.Exit(1)
	}

	// 2. Transfer the listener FD to our parent supervisor
	if err := sendFD(parentSock, listenerFd); err != nil {
		slog.Error("Child failed to send listener FD to parent", "error", err)
		os.Exit(1)
	}
	_ = unix.Close(listenerFd)

	// Block until parent indicates the supervisor loop is ready
	var syncBuf [1]byte
	if _, err := unix.Read(parentSock, syncBuf[:]); err != nil {
		slog.Error("Child failed to read sync byte from parent", "error", err)
		os.Exit(1)
	}
	_ = unix.Close(parentSock)

	// --- ASSERTION 1: Verify Open Interception ---
	file, err := os.Open(mockOpenPath)
	if err != nil {
		slog.Error("Child failed to open mock file", "path", mockOpenPath, "error", err)
		os.Exit(1)
	}
	content, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		slog.Error("Child failed to read mock file content", "error", err)
		os.Exit(1)
	}
	if string(content) != mockOpenContent {
		slog.Error("Open verification failed: Mismatch", "expected", mockOpenContent, "got", string(content))
		os.Exit(1)
	}
	slog.Debug("Child successfully validated dynamic open callback spoofing")

	// --- ASSERTION 2: Verify Fstatat Interception ---
	var stat unix.Stat_t
	err = unix.Fstatat(unix.AT_FDCWD, mockStatPath, &stat, 0)
	if err != nil {
		slog.Error("Child fstatat system call failed", "path", mockStatPath, "error", err)
		os.Exit(1)
	}
	if stat.Ino != mockStatIno {
		slog.Error("Stat verification failed: Inode mismatch", "expected", mockStatIno, "got", stat.Ino)
		os.Exit(1)
	}
	slog.Debug("Child successfully validated fstatat callback spoofing")

	// --- ASSERTION 3: Verify Statx Interception ---
	var statx unix.Statx_t
	err = unix.Statx(unix.AT_FDCWD, mockStatxPath, 0, unix.STATX_BASIC_STATS, &statx)
	if err != nil {
		slog.Error("Child statx system call failed", "path", mockStatxPath, "error", err)
		os.Exit(1)
	}
	if statx.Ino != mockStatxIno {
		slog.Error("Statx verification failed: Inode mismatch", "expected", mockStatxIno, "got", statx.Ino)
		os.Exit(1)
	}
	slog.Debug("Child successfully validated statx callback spoofing")

	// --- ASSERTION 4: Verify Statfs Interception ---
	var statfs unix.Statfs_t
	err = unix.Statfs(mockStatfsPath, &statfs)
	if err != nil {
		slog.Error("Child statfs system call failed", "path", mockStatfsPath, "error", err)
		os.Exit(1)
	}
	if statfs.Type != int64(mockStatfsType) {
		slog.Error("Statfs verification failed: Type mismatch", "expected", mockStatfsType, "got", statfs.Type)
		os.Exit(1)
	}
	slog.Debug("Child successfully validated statfs callback spoofing")

	// --- ASSERTION 5: Verify Readlink Interception ---
	targetPath, err := os.Readlink(mockReadlinkPath)
	if err != nil {
		slog.Error("Child readlink system call failed", "path", mockReadlinkPath, "error", err)
		os.Exit(1)
	}
	if targetPath != mockReadlinkTarget {
		slog.Error("Readlink verification failed: Target mismatch", "expected", mockReadlinkTarget, "got", targetPath)
		os.Exit(1)
	}
	slog.Debug("Child successfully validated readlink callback spoofing")

	os.Exit(0)
}

// SCM_RIGHTS FD passing helpers

func sendFD(socket int, fd int) error {
	rights := unix.UnixRights(fd)
	return unix.Sendmsg(socket, []byte{1}, rights, nil, 0)
}

func recvFD(socket int) (int, error) {
	oob := make([]byte, unix.CmsgSpace(4))
	var b []byte
	_, oobn, _, _, err := unix.Recvmsg(socket, b, oob, 0)
	if err != nil {
		return -1, err
	}
	msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, err
	}
	if len(msgs) == 0 {
		return -1, fmt.Errorf("no control messages")
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil {
		return -1, err
	}
	if len(fds) == 0 {
		return -1, fmt.Errorf("no fds found in payload")
	}
	return fds[0], nil
}

const testChildFilterEnv = "GO_SECCOMP_TEST_FILTER"
