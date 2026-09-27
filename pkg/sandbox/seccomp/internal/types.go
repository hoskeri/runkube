package internal

import (
	"sync"

	"golang.org/x/sys/unix"
)

// Context carries request info (like http.Request)
type Context struct {
	PID     uint32
	Syscall int32     // Actual syscall number (SYS_OPENAT, etc.)
	Method  string    // Normalized method ("OPEN", "STAT", etc.)
	Path    string    // Resolved path
	Args    [6]uint64 // Raw syscall args
	DirFD   int       // Resolved dirfd (AT_FDCWD = -100)
	Flags   uint64    // Open flags (for OPEN)
	Mask    uint32    // Statx mask (for STATX)

	// Response helpers (like http.ResponseWriter)
	Respond Responder
}

// Responder provides typed response methods
type Responder interface {
	// Continue passes syscall to kernel natively
	Continue() error

	// Error returns errno to tracee
	Error(errno unix.Errno) error

	// SpoofFD injects a file descriptor (for OPEN)
	SpoofFD(fd int) error

	// SpoofStat writes stat buffer (for STAT/STATX/STATFS)
	SpoofStat(addr uint64, stat any) error

	// SpoofReadlink writes target to buffer (for READLINK)
	SpoofReadlink(addr uint64, target string, bufSize uint64) error

	// SpoofUname writes a utsname struct (for UNAME)
	SpoofUname(addr uint64, uts *unix.Utsname) error

	// Success returns a custom return value
	Success(ret int64) error
}

// Handler is the core interface (like http.Handler)
type Handler interface {
	ServeSeccomp(ctx *Context)
}

// HandlerFunc adapter for func(Context)
type HandlerFunc func(*Context)

func (f HandlerFunc) ServeSeccomp(ctx *Context) { f(ctx) }

// Middleware wraps handlers (like http middleware)
type Middleware func(Handler) Handler

// HandlerRegistry manages handler registration and dispatch
type HandlerRegistry struct {
	handlers map[int32][]priorityHandler
	mu       sync.RWMutex
}

type priorityHandler struct {
	priority int
	handler  Handler
}

func NewHandlerRegistry() *HandlerRegistry {
	return &HandlerRegistry{
		handlers: make(map[int32][]priorityHandler),
	}
}

func (r *HandlerRegistry) Register(syscall int32, h Handler) {
	priority := 0
	if ph, ok := h.(interface{ Priority() int }); ok {
		priority = ph.Priority()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	list := r.handlers[syscall]
	inserted := false
	for i, existing := range list {
		if priority > existing.priority {
			list = append(list[:i], append([]priorityHandler{{priority, h}}, list[i:]...)...)
			inserted = true
			break
		}
	}
	if !inserted {
		list = append(list, priorityHandler{priority, h})
	}
	r.handlers[syscall] = list
}

func (r *HandlerRegistry) RegisterWithPriority(syscall int32, priority int, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()

	list := r.handlers[syscall]
	inserted := false
	for i, existing := range list {
		if priority > existing.priority {
			list = append(list[:i], append([]priorityHandler{{priority, h}}, list[i:]...)...)
			inserted = true
			break
		}
	}
	if !inserted {
		list = append(list, priorityHandler{priority, h})
	}
	r.handlers[syscall] = list
}

func (r *HandlerRegistry) GetHandlers(syscall int32) []Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()

	list := r.handlers[syscall]
	handlers := make([]Handler, len(list))
	for i, ph := range list {
		handlers[i] = ph.handler
	}
	return handlers
}

func (r *HandlerRegistry) Dispatch(ctx *Context) {
	handlers := r.GetHandlers(ctx.Syscall)

	// Wrap the responder so we can detect whether any handler produced a
	// terminal (non-continue) response. This allows multiple routes for the
	// same syscall to be tried in priority order until one handles the request.
	wrapped := &HandledResponder{Responder: ctx.Respond}
	ctx.Respond = wrapped

	for _, h := range handlers {
		if canHandler, ok := h.(interface{ CanHandle(*Context) bool }); ok {
			if !canHandler.CanHandle(ctx) {
				continue
			}
		}
		h.ServeSeccomp(ctx)
		if wrapped.Handled {
			return
		}
	}

	// No handler produced a terminal response: pass through natively.
	if !wrapped.Handled {
		_ = ctx.Respond.Continue()
	}
}

// HandledResponder tracks if a response was sent
type HandledResponder struct {
	Responder
	Handled bool
}

func (h *HandledResponder) Continue() error {
	h.Handled = false
	return h.Responder.Continue()
}

func (h *HandledResponder) Error(errno unix.Errno) error {
	h.Handled = true
	return h.Responder.Error(errno)
}

func (h *HandledResponder) SpoofFD(fd int) error {
	h.Handled = true
	return h.Responder.SpoofFD(fd)
}

func (h *HandledResponder) SpoofStat(addr uint64, stat any) error {
	h.Handled = true
	return h.Responder.SpoofStat(addr, stat)
}

func (h *HandledResponder) SpoofReadlink(addr uint64, target string, bufSize uint64) error {
	h.Handled = true
	return h.Responder.SpoofReadlink(addr, target, bufSize)
}

func (h *HandledResponder) SpoofUname(addr uint64, uts *unix.Utsname) error {
	h.Handled = true
	return h.Responder.SpoofUname(addr, uts)
}

func (h *HandledResponder) Success(ret int64) error {
	h.Handled = true
	return h.Responder.Success(ret)
}

// PriorityHandler is an optional interface for handlers with priority
type PriorityHandler interface {
	Priority() int
}

// CanHandler is an optional interface for conditional handling
type CanHandler interface {
	CanHandle(*Context) bool
}

// RegistryImpl holds all syscall metadata
type RegistryImpl struct {
	byNumber map[int32]SyscallMeta
	byMethod map[string][]int32
}

func NewRegistryImpl() *RegistryImpl {
	r := &RegistryImpl{
		byNumber: make(map[int32]SyscallMeta),
		byMethod: make(map[string][]int32),
	}
	r.registerDefaults()
	return r
}

func (r *RegistryImpl) registerDefaults() {
	// OPEN family
	r.Register(SyscallMeta{
		Number: unix.SYS_OPEN, Name: "open", Method: "OPEN",
		PathArgIdx: 0, DirFDArgIdx: -1, FlagsArgIdx: 1, ReturnType: ReturnFD,
	})
	r.Register(SyscallMeta{
		Number: unix.SYS_OPENAT, Name: "openat", Method: "OPEN",
		PathArgIdx: 1, DirFDArgIdx: 0, FlagsArgIdx: 2, ReturnType: ReturnFD,
	})
	r.Register(SyscallMeta{
		Number: unix.SYS_OPENAT2, Name: "openat2", Method: "OPEN",
		PathArgIdx: 1, DirFDArgIdx: 0, FlagsArgIdx: 2, MaskArgIdx: 3, ReturnType: ReturnFD,
	})

	// STAT family
	r.Register(SyscallMeta{
		Number: unix.SYS_NEWFSTATAT, Name: "newfstatat", Method: "STAT",
		PathArgIdx: 1, DirFDArgIdx: 0, FlagsArgIdx: 3, ReturnType: ReturnStat,
	})

	// STATX family
	r.Register(SyscallMeta{
		Number: unix.SYS_STATX, Name: "statx", Method: "STATX",
		PathArgIdx: 1, DirFDArgIdx: 0, FlagsArgIdx: 2, MaskArgIdx: 3, ReturnType: ReturnStatx,
	})

	// STATFS family
	r.Register(SyscallMeta{
		Number: unix.SYS_STATFS, Name: "statfs", Method: "STATFS",
		PathArgIdx: 0, DirFDArgIdx: -1, FlagsArgIdx: -1, MaskArgIdx: -1, ReturnType: ReturnStatfs,
	})

	// READLINK family
	r.Register(SyscallMeta{
		Number: unix.SYS_READLINK, Name: "readlink", Method: "READLINK",
		PathArgIdx: 0, DirFDArgIdx: -1, FlagsArgIdx: -1, MaskArgIdx: -1, ReturnType: ReturnReadlink,
	})
	r.Register(SyscallMeta{
		Number: unix.SYS_READLINKAT, Name: "readlinkat", Method: "READLINK",
		PathArgIdx: 1, DirFDArgIdx: 0, FlagsArgIdx: -1, MaskArgIdx: -1, ReturnType: ReturnReadlink,
	})

	// UNAME family (no path argument: arg0 is a pointer to struct utsname)
	r.Register(SyscallMeta{
		Number: unix.SYS_UNAME, Name: "uname", Method: "UNAME",
		PathArgIdx: -1, DirFDArgIdx: -1, FlagsArgIdx: -1, MaskArgIdx: -1, ReturnType: ReturnUname,
	})
}

func (r *RegistryImpl) Register(meta SyscallMeta) {
	r.byNumber[meta.Number] = meta
	r.byMethod[meta.Method] = append(r.byMethod[meta.Method], meta.Number)
}

func (r *RegistryImpl) Lookup(nr int32) *SyscallMeta {
	if meta, ok := r.byNumber[nr]; ok {
		return &meta
	}
	return nil
}

func (r *RegistryImpl) SyscallsForMethod(method string) []int32 {
	return r.byMethod[method]
}

func (r *RegistryImpl) AllInterceptedSyscalls() []int32 {
	var syscalls []int32
	for nr := range r.byNumber {
		syscalls = append(syscalls, nr)
	}
	return syscalls
}

// Registry interface for syscall metadata lookup
type Registry interface {
	Lookup(nr int32) *SyscallMeta
	SyscallsForMethod(method string) []int32
	AllInterceptedSyscalls() []int32
}

// SyscallMeta describes a syscall for filter generation and argument parsing
type SyscallMeta struct {
	Number      int32
	Name        string
	Method      string     // Normalized method: OPEN, STAT, STATX, STATFS, READLINK, UNAME
	PathArgIdx  int        // Which arg holds the path pointer (-1 if none)
	DirFDArgIdx int        // Which arg holds dirfd for *at syscalls (-1 if none)
	FlagsArgIdx int        // Which arg holds flags (for openat2, etc.)
	MaskArgIdx  int        // Which arg holds mask (for statx)
	ReturnType  ReturnType // How to handle return value
}

// ReturnType describes how the syscall returns data
type ReturnType int

const (
	ReturnFD       ReturnType = iota // Returns file descriptor (open family)
	ReturnInt                        // Returns int (most syscalls)
	ReturnStat                       // Writes to struct pointer (stat family)
	ReturnStatx                      // Writes to statx struct (statx)
	ReturnStatfs                     // Writes to statfs struct (statfs)
	ReturnReadlink                   // Writes to buffer (readlink family)
	ReturnUname                      // Writes to utsname struct (uname)
)

// SupervisorConfig holds the configuration for the supervisor loop
type SupervisorConfig struct {
	HandlerRegistry *HandlerRegistry
	SyscallRegistry Registry
}
