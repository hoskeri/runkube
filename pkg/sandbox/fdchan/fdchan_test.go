package fdchan

import (
	"bytes"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// TestParentChildRoundTrip mimics the fork/exec lifecycle using two connected
// components to verify data multiplexing and descriptor transferring.
func TestParentChildRoundTrip(t *testing.T) {
	// 1. Stand up the Parent engine
	parent, err := NewParent()
	if err != nil {
		t.Fatalf("Failed to create Parent: %v", err)
	}
	defer parent.Close()

	// 2. Extract the child side file handle manually (mimicking your updated API workflow)
	childFD := parent.ChildFD()
	if childFD <= 0 {
		t.Fatalf("Expected valid child FD, got %d", childFD)
	}

	childFile := os.NewFile(uintptr(childFD), "child-mock-pipe")

	// Convert it to a net.UnixConn to spin up a mock Child client in a goroutine
	cConn, err := net.FileConn(childFile)
	if err != nil {
		t.Fatalf("Failed to convert child file to connection: %v", err)
	}

	// We can safely close the parent's file track to the child now
	parent.closeChildHandle()

	childMux := newMux(cConn.(*net.UnixConn))
	childMock := &Child{mux: childMux}
	defer childMock.Close()

	// 3. Create temp files to pass back and forth
	parentSentFile, err := os.CreateTemp("", "parent-send-test-*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(parentSentFile.Name())
	defer parentSentFile.Close()

	_, _ = parentSentFile.WriteString("Secret payload from parent")
	_, _ = parentSentFile.Seek(0, 0)

	childSentFile, err := os.CreateTemp("", "child-send-test-*.txt")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(childSentFile.Name())
	defer childSentFile.Close()

	_, _ = childSentFile.WriteString("Response payload from child")
	_, _ = childSentFile.Seek(0, 0)

	// 4. Spin up the background mock child loop
	childErrChan := make(chan error, 1)
	go func() {
		// Block and wait for parent's request
		reqMsg, respond, err := childMock.HandleRequest()
		if err != nil {
			childErrChan <- err
			return
		}

		// Verify request data payload
		if !bytes.Equal(reqMsg.Data, []byte("Parent Request Data")) {
			childErrChan <- fatedError("unexpected request data: %s", reqMsg.Data)
			return
		}

		// Pull the passed descriptor dropped by the parent
		f, purpose, err := reqMsg.GetDescriptor("p-file-1")
		if err != nil {
			childErrChan <- err
			return
		}
		defer f.Close()

		if purpose != "Parent validation data" {
			childErrChan <- fatedError("unexpected purpose: %s", purpose)
			return
		}

		// Read content from the passed descriptor to verify it's the exact same file structural handle
		content, err := io.ReadAll(f)
		if err != nil || string(content) != "Secret payload from parent" {
			childErrChan <- fatedError("failed reading inherited file content: %v", err)
			return
		}

		// Craft response message package to send back up the pipe
		reply := Message{Data: []byte("Child Response Data")}
		_ = reply.AddDescriptor(childSentFile, "c-file-reply", "Child receipt validation")

		if err := respond(reply); err != nil {
			childErrChan <- err
			return
		}

		close(childErrChan)
	}()

	// 5. Parent initiates the transaction
	msg := Message{Data: []byte("Parent Request Data")}
	if err := msg.AddDescriptor(parentSentFile, "p-file-1", "Parent validation data"); err != nil {
		t.Fatalf("Failed to add descriptor: %v", err)
	}

	// Blocks synchronously until response comes back from the background worker goroutine
	response, err := parent.Request(msg)
	if err != nil {
		t.Fatalf("Parent request failed: %v", err)
	}

	// 6. Assertions on Parent response parsing
	if !bytes.Equal(response.Data, []byte("Child Response Data")) {
		t.Errorf("Expected 'Child Response Data', got %s", response.Data)
	}

	respFile, respPurpose, err := response.GetDescriptor("c-file-reply")
	if err != nil {
		t.Fatalf("Parent failed to extract response file: %v", err)
	}
	defer respFile.Close()

	if respPurpose != "Child receipt validation" {
		t.Errorf("Unexpected response purpose: %s", respPurpose)
	}

	respContent, err := io.ReadAll(respFile)
	if err != nil || string(respContent) != "Response payload from child" {
		t.Errorf("Failed reading file passed back from child: %v", err)
	}

	// 7. Check if child runtime ran into any asynchronous execution constraints
	select {
	case childErr := <-childErrChan:
		if childErr != nil {
			t.Errorf("Child goroutine failed: %v", childErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Test timed out waiting for child goroutine to finish")
	}
}

// TestMessageValidation confirms constraints like duplicate IDs and missing values are caught early.
func TestMessageValidation(t *testing.T) {
	msg := &Message{Data: []byte("test")}
	dummyFile := os.Stdout

	if err := msg.AddDescriptor(nil, "id", "purpose"); err == nil {
		t.Error("Expected error when adding a nil file pointer, got none")
	}

	if err := msg.AddDescriptor(dummyFile, "", "purpose"); err == nil {
		t.Error("Expected error when adding an empty ID string, got none")
	}

	err := msg.AddDescriptor(dummyFile, "duplicate-id", "first")
	if err != nil {
		t.Fatalf("Unexpected error adding first descriptor: %v", err)
	}

	// Try adding another descriptor using the identical ID index key
	err = msg.AddDescriptor(dummyFile, "duplicate-id", "second")
	if err == nil {
		t.Error("Expected collision error when adding duplicate identity key, got none")
	}

	_, _, err = msg.GetDescriptor("non-existent")
	if err == nil {
		t.Error("Expected error looking up missing ID, got none")
	}
}

func fatedError(format string, args ...interface{}) error {
	return (error)(io.ErrUnexpectedEOF) // fallback mock type wrapper helper
}
