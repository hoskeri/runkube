package fdchan

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
)

// EnvVarKey should be set in the child process point to ChildFD.
const EnvVarKey = "FDCHAN_INHERITED_FD"

type annotation struct {
	ID      string `json:"id"`
	Purpose string `json:"purpose"`
}

type payload struct {
	Sequence    uint64       `json:"seq"`
	IsResponse  bool         `json:"is_response"`
	Annotations []annotation `json:"annotations,omitempty"`
	Data        []byte       `json:"data,omitempty"`
}

// Message is the public interface users interact with.
type Message struct {
	Data        []byte
	files       []*os.File
	annotations []annotation // Private tracking schema
	seq         uint64       // Private sequence tracker to pair async child responses
}

// AddDescriptor binds a file handle with an explicit identity and purpose to the message package.
func (m *Message) AddDescriptor(fd *os.File, id, purpose string) error {
	if fd == nil {
		return errors.New("cannot add a nil file descriptor")
	}
	if id == "" {
		return errors.New("descriptor identity ID cannot be empty")
	}

	// Guard against duplicate IDs inside the same message frame
	for _, annot := range m.annotations {
		if annot.ID == id {
			return fmt.Errorf("descriptor with ID %q already registered in this message", id)
		}
	}

	m.files = append(m.files, fd)
	m.annotations = append(m.annotations, annotation{
		ID:      id,
		Purpose: purpose,
	})
	return nil
}

// GetDescriptor scans the payload metadata for the requested ID.
// If found, it returns the file handle and its documented purpose context.
func (m *Message) GetDescriptor(id string) (*os.File, string, error) {
	for i, annot := range m.annotations {
		if annot.ID == id {
			if i >= len(m.files) {
				return nil, "", errors.New("integrity violation: metadata index out of bounds of file allocation table")
			}
			return m.files[i], annot.Purpose, nil
		}
	}
	return nil, "", fmt.Errorf("descriptor handle with identity %q not found in message payload", id)
}

// Result binds a payload and its companion files into a single channel delivery unit.
type Result struct {
	payload payload
	files   []*os.File
}

// session represents a pending synchronous transaction waiting for a response.
type session struct {
	ch chan Result
}

// mux manages full-duplex multiplexing over the raw UNIX socket.
type mux struct {
	conn       *net.UnixConn
	nextSeq    uint64
	writeMu    sync.Mutex
	sessionsMu sync.Mutex
	sessions   map[uint64]*session
	inboundRx  chan Message
	closeOnce  sync.Once
	done       chan struct{}
}

func newMux(conn *net.UnixConn) *mux {
	m := &mux{
		conn:      conn,
		sessions:  make(map[uint64]*session),
		inboundRx: make(chan Message, 128),
		done:      make(chan struct{}),
	}
	go m.readLoop()
	return m
}

func (m *mux) readLoop() {
	defer close(m.done)
	for {
		p, files, err := readWire(m.conn)
		if err != nil {
			m.sessionsMu.Lock()
			for _, s := range m.sessions {
				close(s.ch)
			}
			m.sessions = make(map[uint64]*session)
			m.sessionsMu.Unlock()
			return
		}

		if p.IsResponse {
			m.sessionsMu.Lock()
			s, exists := m.sessions[p.Sequence]
			if exists {
				delete(m.sessions, p.Sequence)
				s.ch <- Result{payload: p, files: files}
			} else {
				closeAll(files)
			}
			m.sessionsMu.Unlock()
		} else {
			m.inboundRx <- Message{
				Data:        p.Data,
				files:       files,
				annotations: p.Annotations,
				seq:         p.Sequence,
			}
		}
	}
}

func (m *mux) Request(msg Message) (Message, error) {
	seq := atomic.AddUint64(&m.nextSeq, 1)
	resChan := make(chan Result, 1)

	m.sessionsMu.Lock()
	m.sessions[seq] = &session{ch: resChan}
	m.sessionsMu.Unlock()

	p := payload{
		Sequence:    seq,
		IsResponse:  false,
		Annotations: msg.annotations,
		Data:        msg.Data,
	}

	m.writeMu.Lock()
	err := writeWire(m.conn, p, msg.files)
	m.writeMu.Unlock()

	if err != nil {
		m.sessionsMu.Lock()
		delete(m.sessions, seq)
		m.sessionsMu.Unlock()
		return Message{}, err
	}

	res, ok := <-resChan
	if !ok {
		return Message{}, errors.New("connection reset while waiting for response")
	}

	return Message{
		Data:        res.payload.Data,
		files:       res.files,
		annotations: res.payload.Annotations,
		seq:         res.payload.Sequence,
	}, nil
}

func (m *mux) receiveRequest() (Message, error) {
	select {
	case msg, ok := <-m.inboundRx:
		if !ok {
			return Message{}, errors.New("channel multiplexer closed")
		}
		return msg, nil
	case <-m.done:
		return Message{}, errors.New("channel multiplexer terminated")
	}
}

func (m *mux) closeConn() error {
	var err error
	m.closeOnce.Do(func() {
		err = m.conn.Close()
		<-m.done
	})
	return err
}

// Parent is an RPC handle to a child fd.
type Parent struct {
	mux       *mux
	childFile *os.File
}

// NewParent creats a new Parent
func NewParent() (*Parent, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("failed creating socketpair: %w", err)
	}

	parentFile := os.NewFile(uintptr(fds[0]), "parent-ipc")
	childFile := os.NewFile(uintptr(fds[1]), "child-ipc")

	pConn, err := net.FileConn(parentFile)
	parentFile.Close()
	if err != nil {
		childFile.Close()
		return nil, fmt.Errorf("failed parent initialization: %w", err)
	}

	return &Parent{
		mux:       newMux(pConn.(*net.UnixConn)),
		childFile: childFile,
	}, nil
}

// ChildFD returns the underlying raw integer descriptor for the child's side
// of the socketpair. Pass this handle directly to your fork/exec lifecycle.
func (p *Parent) ChildFD() int {
	if p.childFile == nil {
		return -1
	}
	return int(p.childFile.Fd())
}

// EnvPair returns the standard environment variable key-value string mapping
// required by the Child library context, given the targeted FD index inside the child.
func (p *Parent) EnvPair(targetIndex int) string {
	return fmt.Sprintf("%s=%d", EnvVarKey, targetIndex)
}

func (p *Parent) closeChildHandle() {
	if p.childFile != nil {
		_ = p.childFile.Close()
		p.childFile = nil
	}
}

func (p *Parent) Request(msg Message) (Message, error) {
	return p.mux.Request(msg)
}

func (p *Parent) Close() error {
	if p.childFile != nil {
		_ = p.childFile.Close()
	}
	return p.mux.closeConn()
}

// Child is a connection to a parent
type Child struct {
	mux *mux
}

// NewChild creates a new child
func NewChild() (*Child, error) {
	fdStr := os.Getenv(EnvVarKey)
	if fdStr == "" {
		return nil, fmt.Errorf("runtime context missing: %s variable not found", EnvVarKey)
	}

	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return nil, fmt.Errorf("corrupted file descriptor context: %w", err)
	}

	f := os.NewFile(uintptr(fd), "child-inherited-ipc")
	if f == nil {
		return nil, errors.New("failed to parse inherited handle")
	}
	defer f.Close()

	cConn, err := net.FileConn(f)
	if err != nil {
		return nil, fmt.Errorf("failed net conversion: %w", err)
	}

	return &Child{
		mux: newMux(cConn.(*net.UnixConn)),
	}, nil
}

// HandleRequest waits for a single request.
func (c *Child) HandleRequest() (Message, func(Message) error, error) {
	reqMsg, err := c.mux.receiveRequest()
	if err != nil {
		return Message{}, nil, err
	}

	responder := func(reply Message) error {
		respPayload := payload{
			Sequence:    reqMsg.seq,
			IsResponse:  true,
			Annotations: reply.annotations,
			Data:        reply.Data,
		}
		c.mux.writeMu.Lock()
		defer c.mux.writeMu.Unlock()
		return writeWire(c.mux.conn, respPayload, reply.files)
	}

	return reqMsg, responder, nil
}

// Close the child multiplexer
func (c *Child) Close() error {
	return c.mux.closeConn()
}

func writeWire(conn *net.UnixConn, p payload, fds []*os.File) error {
	if len(fds) != len(p.Annotations) {
		return errors.New("mismatched count between provided FDs and internal annotations")
	}

	metaBytes, err := json.Marshal(p)
	if err != nil {
		return err
	}

	buf := make([]byte, 4+len(metaBytes))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(metaBytes)))
	copy(buf[4:], metaBytes)

	rawFDs := make([]int, len(fds))
	for i, f := range fds {
		rawFDs[i] = int(f.Fd())
	}

	var oob []byte
	if len(rawFDs) > 0 {
		oob = syscall.UnixRights(rawFDs...)
	}

	_, _, err = conn.WriteMsgUnix(buf, oob, nil)
	return err
}

func readWire(conn *net.UnixConn) (payload, []*os.File, error) {
	headerBuf := make([]byte, 4)
	oobBuf := make([]byte, syscall.CmsgSpace(10*4))

	n, oobN, _, _, err := conn.ReadMsgUnix(headerBuf, oobBuf)
	if err != nil {
		return payload{}, nil, err
	}
	if n < 4 {
		return payload{}, nil, errors.New("incomplete frame header")
	}

	rawFDs, err := parseFDs(oobBuf[:oobN])
	if err != nil {
		return payload{}, nil, err
	}

	files := make([]*os.File, len(rawFDs))
	for i, fd := range rawFDs {
		files[i] = os.NewFile(uintptr(fd), "wire-fd")
	}

	metaLen := binary.BigEndian.Uint32(headerBuf)
	bodyBuf := make([]byte, metaLen)
	if _, err := conn.Read(bodyBuf); err != nil {
		closeAll(files)
		return payload{}, nil, err
	}

	var p payload
	if err := json.Unmarshal(bodyBuf, &p); err != nil {
		closeAll(files)
		return payload{}, nil, err
	}

	return p, files, nil
}

func parseFDs(oob []byte) ([]int, error) {
	if len(oob) == 0 {
		return nil, nil
	}
	cmsgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, cmsg := range cmsgs {
		fdsPart, err := syscall.ParseUnixRights(&cmsg)
		if err != nil {
			return nil, err
		}
		fds = append(fds, fdsPart...)
	}
	return fds, nil
}

func closeAll(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
