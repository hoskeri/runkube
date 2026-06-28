package seccomp

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	envVarTestChild = "GO_WANT_SECCOMP_TEST_CHILD"
	testMockPath    = "/proc/meminfo"
	testMockData    = "MemTotal:       123456 kB\n"
)

// TestSupervise verifies that the supervisor can intercept the child's open/openat calls
// and atomically inject a spoofed memory-backed file descriptor.
func TestSupervise(t *testing.T) {
	// If we are currently running as the child process under test, handle child tasks.
	if os.Getenv(envVarTestChild) == "1" {
		runTestChild()
		return
	}

	// Create a socket pair for sync and passing the seccomp listener FD
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("Failed to create Socketpair: %v", err)
	}
	parentSock := fds[0]
	childSock := fds[1]

	// Re-execute this test binary to spawn the child in a user namespace
	cmd := exec.Command(os.Args[0], "-test.run=TestSupervise")
	cmd.Env = append(os.Environ(), fmt.Sprintf("%s=1", envVarTestChild))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
	}
	// Pass the child end of socketpair on FD 3
	cmd.ExtraFiles = []*os.File{os.NewFile(uintptr(childSock), "child_sock")}

	if err := cmd.Start(); err != nil {
		t.Fatalf("Failed to start child process: %v", err)
	}
	_ = unix.Close(childSock)

	// Receive the seccomp listener FD from child via SCM_RIGHTS
	listenerFd, err := recvFD(parentSock)
	if err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("Failed to receive seccomp listener FD: %v", err)
	}
	defer unix.Close(listenerFd)

	// Spin up the supervisor in a separate goroutine
	mockFiles := map[string]string{
		testMockPath: testMockData,
	}
	errChan := make(chan error, 1)
	go func() {
		errChan <- Supervise(listenerFd, mockFiles)
	}()

	// Signal child that supervisor loop is ready
	if _, err := unix.Write(parentSock, []byte{1}); err != nil {
		t.Fatalf("Failed to write synchronization byte to child: %v", err)
	}
	_ = unix.Close(parentSock)

	// Wait for child process to finish its assertions and exit
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Child process returned with an error: %v", err)
	}

	// Verify the supervisor gracefully shut down or is ready to shut down
	_ = unix.Close(listenerFd)
	superviseErr := <-errChan
	if superviseErr != nil {
		t.Logf("Supervisor exited with expected descriptor close error: %v", superviseErr)
	}
}

// runTestChild executes inside the spawned, namespaced child process.
func runTestChild() {
	// Child receives socketpair end on FD 3
	parentSock := 3

	// Install the filter
	listenerFd, err := InstallFilter()
	if err != nil {
		slog.Error("Child failed to install seccomp filter", "error", err)
		os.Exit(1)
	}

	// Send seccomp notify listener FD to the parent
	if err := sendFD(parentSock, listenerFd); err != nil {
		slog.Error("Child failed to send listener FD to parent", "error", err)
		os.Exit(1)
	}
	_ = unix.Close(listenerFd)

	// Synchronize with parent supervisor
	var syncBuf [1]byte
	if _, err := unix.Read(parentSock, syncBuf[:]); err != nil {
		slog.Error("Child failed to read sync byte from parent", "error", err)
		os.Exit(1)
	}
	_ = unix.Close(parentSock)

	// Test 1: Intercepted read via openat(2) (using standard Go library)
	testReadOpenat()

	// Test 2: Intercepted read via legacy open(2) syscall directly
	testReadLegacyOpen()

	slog.Debug("Child successfully verified all spoofed fd scenarios")
	os.Exit(0)
}

func testReadOpenat() {
	file, err := os.Open(testMockPath)
	if err != nil {
		slog.Error("Child failed to open mocked path with openat", "path", testMockPath, "error", err)
		os.Exit(1)
	}
	defer file.Close()

	content, err := io.ReadAll(file)
	if err != nil {
		slog.Error("Child failed to read intercepted openat fd content", "error", err)
		os.Exit(1)
	}

	if string(content) != testMockData {
		slog.Error("Verification failed on openat: Content mismatch", "expected", testMockData, "got", string(content))
		os.Exit(1)
	}
}

func testReadLegacyOpen() {
	// Convert path to NULL-terminated C-style string
	cPath, err := unix.BytePtrFromString(testMockPath)
	if err != nil {
		slog.Error("Failed to convert mock path to byte pointer", "error", err)
		os.Exit(1)
	}

	// Execute a raw legacy open(2) system call
	fd, _, sysErr := unix.Syscall(
		unix.SYS_OPEN,
		uintptr(unsafe.Pointer(cPath)),
		uintptr(unix.O_RDONLY),
		0,
	)
	if sysErr != 0 {
		slog.Error("Child raw open(2) system call failed", "errno", sysErr)
		os.Exit(1)
	}
	defer unix.Close(int(fd))

	file := os.NewFile(fd, testMockPath)
	content, err := io.ReadAll(file)
	if err != nil {
		slog.Error("Child failed to read legacy open(2) raw fd", "error", err)
		os.Exit(1)
	}

	if string(content) != testMockData {
		slog.Error("Verification failed on legacy open: Content mismatch", "expected", testMockData, "got", string(content))
		os.Exit(1)
	}
}

// SCM_RIGHTS FD passing helpers

func sendFD(socket int, fd int) error {
	rights := unix.UnixRights(fd)
	return unix.Sendmsg(socket, nil, rights, nil, 0)
}

func recvFD(socket int) (int, error) {
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := unix.Recvmsg(socket, nil, oob, 0)
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
