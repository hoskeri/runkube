# Seccomp Handler Interface - HTTP Mux Style Design

## Core Concept

Treat syscalls like HTTP methods and paths like URLs. Use a mux/router pattern:

```go
mux := seccomp.NewMux()

// Exact path matches
mux.Handle("OPEN", "/proc/mounts", seccomp.SpoofFile(staticMountData))
mux.Handle("OPEN", "/proc/swaps", seccomp.SpoofFile(staticSwaps))

// Prefix matches (like HTTP path prefixes)
mux.HandlePrefix("OPEN", "/fake/", seccomp.SpoofDir(fakeFS))

// Regex/glob matches
mux.HandlePattern("STAT", "/proc/\\d+/maps", seccomp.SpoofStat(procMapsStat))

// Fallback / catch-all
mux.Handle("OPEN", "*", seccomp.PassThrough())

// Middleware-style chaining
mux.Use(seccomp.LoggingMiddleware())
mux.Use(seccomp.MetricsMiddleware())

// Build filter + supervisor from mux
filter, supervisor := mux.Build()
```

---

## API Design

### 1. The Mux (Central Router)

```go
package seccomp

// Mux routes syscall+path to handlers, like http.ServeMux
type Mux struct {
    routes    []route
    middlewares []Middleware
    registry  *SyscallRegistry
}

func NewMux() *Mux {
    return &Mux{
        registry: DefaultRegistry,
    }
}

// Handle registers a handler for exact syscall+path match
// method: "OPEN", "OPENAT", "OPENAT2", "STAT", "STATX", "STATFS", "READLINK", "READLINKAT"
// path: exact path like "/proc/mounts" or "*" for catch-all
func (m *Mux) Handle(method, path string, h Handler) *Mux {
    m.routes = append(m.routes, route{
        method:  method,
        path:    path,
        pattern: nil,
        handler: h,
        exact:   true,
    })
    return m
}

// HandlePrefix matches path prefixes (e.g., "/fake/" matches "/fake/file.txt")
func (m *Mux) HandlePrefix(method, prefix string, h Handler) *Mux {
    m.routes = append(m.routes, route{
        method:  method,
        path:    prefix,
        pattern: nil,
        handler: h,
        exact:   false,
    })
    return m
}

// HandlePattern registers a regex pattern
func (m *Mux) HandlePattern(method, pattern string, h Handler) *Mux {
    re, _ := regexp.Compile(pattern)
    m.routes = append(m.routes, route{
        method:  method,
        path:    pattern,
        pattern: re,
        handler: h,
        exact:   false,
    })
    return m
}

// Use adds middleware (executed in order before handler)
func (m *Mux) Use(mw Middleware) *Mux {
    m.middlewares = append(m.middlewares, mw)
    return m
}

// Build compiles the mux into a Filter + SupervisorConfig
func (m *Mux) Build() (Filter, SupervisorConfig) {
    // 1. Collect unique syscalls from routes
    syscalls := m.collectSyscalls()
    
    // 2. Build BPF filter
    filter := NewFilterBuilder(m.registry).Intercept(syscalls...).Build()
    
    // 3. Build handler registry with middleware chain
    handlerReg := m.buildHandlerRegistry()
    
    return filter, SupervisorConfig{
        HandlerRegistry: handlerReg,
        SyscallRegistry: m.registry,
    }
}
```

### 2. Syscall Methods (HTTP-like)

```go
// Method constants - map to syscall families
const (
    MethodOpen     = "OPEN"     // open, openat, openat2
    MethodStat     = "STAT"     // newfstatat (fstatat)
    MethodStatx    = "STATX"    // statx
    MethodStatfs   = "STATFS"   // statfs
    MethodReadlink = "READLINK" // readlink, readlinkat
)

// All methods for convenience
var AllMethods = []string{MethodOpen, MethodStat, MethodStatx, MethodStatfs, MethodReadlink}

// MethodToSyscalls maps method to syscall numbers for filter building
var MethodToSyscalls = map[string][]int32{
    MethodOpen:     {unix.SYS_OPEN, unix.SYS_OPENAT, unix.SYS_OPENAT2},
    MethodStat:     {unix.SYS_NEWFSTATAT},
    MethodStatx:    {unix.SYS_STATX},
    MethodStatfs:   {unix.SYS_STATFS},
    MethodReadlink: {unix.SYS_READLINK, unix.SYS_READLINKAT},
}
```

### 3. Handler Interface (Simple, Composable)

```go
// Context carries request info (like http.Request)
type Context struct {
    PID       uint32
    Syscall   int32       // Actual syscall number (SYS_OPENAT, etc.)
    Method    string      // Normalized method ("OPEN", "STAT", etc.)
    Path      string      // Resolved path
    Args      [6]uint64   // Raw syscall args
    DirFD     int         // Resolved dirfd (AT_FDCWD = -100)
    Flags     uint64      // Open flags (for OPEN)
    Mask      uint32      // Statx mask (for STATX)
    
    // Response helpers (like http.ResponseWriter)
    Respond   Responder
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
```

### 4. Built-in Handlers (Convenience Constructors)

```go
// SpoofFile returns a handler that serves a file from memory
func SpoofFile(content string) Handler {
    return HandlerFunc(func(ctx *Context) {
        memfd, _ := unix.MemfdCreate("spoof", unix.MFD_CLOEXEC)
        unix.Write(memfd, []byte(content))
        unix.Seek(memfd, 0, io.SeekStart)
        ctx.Respond.SpoofFD(memfd)
        unix.Close(memfd) // Safe: kernel holds reference via SECCOMP_ADDFD
    })
}

// SpoofFileFunc returns handler with dynamic content
func SpoofFileFunc(fn func(path string) (string, error)) Handler {
    return HandlerFunc(func(ctx *Context) {
        content, err := fn(ctx.Path)
        if err != nil {
            ctx.Respond.Error(unix.EIO)
            return
        }
        if content == "" {
            ctx.Respond.Continue()
            return
        }
        memfd, _ := unix.MemfdCreate("spoof", unix.MFD_CLOEXEC)
        unix.Write(memfd, []byte(content))
        unix.Seek(memfd, 0, io.SeekStart)
        ctx.Respond.SpoofFD(memfd)
        unix.Close(memfd)
    })
}

// SpoofStat returns handler that fakes stat/statx/statfs
func SpoofStat(stat any) Handler {
    return HandlerFunc(func(ctx *Context) {
        switch ctx.Method {
        case MethodStat, MethodStatx:
            // Arg[2] for STAT, Arg[4] for STATX
            addr := ctx.Args[2]
            if ctx.Method == MethodStatx {
                addr = ctx.Args[4]
            }
            ctx.Respond.SpoofStat(addr, stat)
        case MethodStatfs:
            // Arg[1] for statfs
            ctx.Respond.SpoofStat(ctx.Args[1], stat)
        default:
            ctx.Respond.Continue()
        }
    })
}

// SpoofReadlink returns handler that fakes readlink
func SpoofReadlink(target string) Handler {
    return HandlerFunc(func(ctx *Context) {
        // Arg[1] for readlink, Arg[2] for readlinkat
        bufAddr := ctx.Args[1]
        bufSize := ctx.Args[2]
        if ctx.Syscall == unix.SYS_READLINKAT {
            bufAddr = ctx.Args[2]
            bufSize = ctx.Args[3]
        }
        ctx.Respond.SpoofReadlink(bufAddr, target, bufSize)
    })
}

// PassThrough continues natively (explicit no-op)
func PassThrough() Handler {
    return HandlerFunc(func(ctx *Context) {
        ctx.Respond.Continue()
    })
}

// Reject returns error (like 404)
func Reject(errno unix.Errno) Handler {
    return HandlerFunc(func(ctx *Context) {
        ctx.Respond.Error(errno)
    })
}

// Chain combines multiple handlers (first to handle wins)
func Chain(handlers ...Handler) Handler {
    return HandlerFunc(func(ctx *Context) {
        for _, h := range handlers {
            // Wrap responder to detect if handled
            wrapped := &handledResponder{Responder: ctx.Respond}
            ctx.Respond = wrapped
            h.ServeSeccomp(ctx)
            if wrapped.handled {
                return
            }
        }
    })
}
```

### 5. Middleware (Cross-cutting Concerns)

```go
// Middleware wraps handlers (like http middleware)
type Middleware func(Handler) Handler

// LoggingMiddleware logs all intercepted syscalls
func LoggingMiddleware() Middleware {
    return func(next Handler) Handler {
        return HandlerFunc(func(ctx *Context) {
            slog.Debug("seccomp intercept", "method", ctx.Method, "path", ctx.Path, "pid", ctx.PID)
            next.ServeSeccomp(ctx)
        })
    }
}

// MetricsMiddleware records metrics
func MetricsMiddleware() Middleware {
    return func(next Handler) Handler {
        return HandlerFunc(func(ctx *Context) {
            start := time.Now()
            next.ServeSeccomp(ctx)
            duration := time.Since(start)
            metrics.Record(ctx.Method, ctx.Path, duration)
        })
    }
}

// PathRewriteMiddleware rewrites paths before handler
func PathRewriteMiddleware(rewrites map[string]string) Middleware {
    return func(next Handler) Handler {
        return HandlerFunc(func(ctx *Context) {
            if newPath, ok := rewrites[ctx.Path]; ok {
                ctx.Path = newPath
            }
            next.ServeSeccomp(ctx)
        })
    }
}

// ConditionalMiddleware only applies handler if condition matches
func ConditionalMiddleware(cond func(*Context) bool, mw Middleware) Middleware {
    return func(next Handler) Handler {
        return HandlerFunc(func(ctx *Context) {
            if cond(ctx) {
                mw(next).ServeSeccomp(ctx)
            } else {
                next.ServeSeccomp(ctx)
            }
        })
    }
}
```

---

## Usage Examples

### Basic Setup (Replaces Current Code)

```go
func (s *Sandbox) supervisorConfig() (seccomp.Filter, seccomp.SupervisorConfig) {
    mux := seccomp.NewMux()
    
    // Static file spoofing
    mux.Handle("OPEN", "/proc/mounts", seccomp.SpoofFile(staticMountData))
    mux.Handle("OPEN", "/proc/swaps", seccomp.SpoofFile(staticSwaps))
    
    // Dynamic handler
    mux.Handle("OPEN", "/var/run/secrets/*", seccomp.SpoofFileFunc(func(path string) (string, error) {
        return readSecret(path)
    }))
    
    // Fake stat for specific path
    mux.Handle("STAT", "/fake/stat", seccomp.SpoofStat(&unix.Stat_t{
        Ino: 12345, Mode: unix.S_IFREG | 0644, Size: 100,
    }))
    
    // Catch-all passthrough (optional - implicit if no match)
    mux.Handle("OPEN", "*", seccomp.PassThrough())
    
    // Middleware
    mux.Use(seccomp.LoggingMiddleware())
    
    return mux.Build()
}

// In sandbox.go: the child installs the filter during the single setup RPC
// and returns the listener FD in the setup response; the parent supervises it.
func (s *Sandbox) installSyscallFilter() (int, error) {
    return seccomp.InstallFilter()
}
// ... child adds the returned FD to the setup response (id "seccomp-fd") ...
func (s *Sandbox) superviseSyscallFilter(lfd *os.File) error {
    config := s.supervisorConfig()
    go seccomp.Supervise(lfd, config)
}
```

### Advanced: Virtual Filesystem

```go
// A virtual filesystem handler
type VFSHandler struct {
    fs map[string]string // path -> content
}

func (v *VFSHandler) ServeSeccomp(ctx *Context) {
    switch ctx.Method {
    case "OPEN":
        if content, ok := v.fs[ctx.Path]; ok {
            ctx.Respond.SpoofFile(content)
            return
        }
    case "STAT", "STATX":
        if _, ok := v.fs[ctx.Path]; ok {
            ctx.Respond.SpoofStat(ctx.Args[2], &unix.Stat_t{
                Ino: 1, Mode: unix.S_IFREG | 0444, Size: int64(len(v.fs[ctx.Path])),
            })
            return
        }
    }
    ctx.Respond.Continue() // Not found -> pass through
}

// Usage:
vfs := &VFSHandler{
    fs: map[string]string{
        "/etc/custom-config": "custom=value\n",
        "/etc/version":       "1.0.0\n",
    }
}
mux.HandlePrefix("OPEN", "/etc/", vfs)
mux.HandlePrefix("STAT", "/etc/", vfs)
```

### Advanced: Middleware Chain

```go
mux := seccomp.NewMux()

// Global middleware
mux.Use(seccomp.LoggingMiddleware())
mux.Use(seccomp.MetricsMiddleware())

// Conditional middleware: only log errors for sensitive paths
mux.Use(seccomp.ConditionalMiddleware(
    func(ctx *Context) bool { return strings.HasPrefix(ctx.Path, "/etc/") },
    seccomp.AuditMiddleware(),
))

// Route-specific middleware via Chain
mux.Handle("OPEN", "/proc/mounts", seccomp.Chain(
    seccomp.RateLimitMiddleware(10), // Max 10 opens/sec
    seccomp.SpoofFile(staticMountData),
))
```

---

## Implementation Plan

### Files to Create

```
pkg/sandbox/seccomp/
├── mux.go              # Mux, Route, Build()
├── context.go          # Context, Responder
├── handler.go          # Handler interface, built-in handlers
├── middleware.go       # Middleware types, built-in middleware
├── filter/
│   ├── builder.go      # FilterBuilder (from DESIGN.md)
│   └── registry.go     # SyscallRegistry, SyscallMeta
├── supervisor/
│   ├── supervisor.go   # Supervise() - main loop
│   ├── dispatcher.go   # Matches route -> calls handler
│   └── memory.go       # process_vm_readv/writev
├── config.go           # SupervisorConfig (new, minimal)
└── seccomp.go          # Public API: InstallFilter, Supervise
```

### Key Implementation Details

**Dispatcher** (replaces current `Supervise` loop):

```go
func (m *Mux) buildHandlerRegistry() *HandlerRegistry {
    reg := NewHandlerRegistry()
    
    // Convert routes to handlers with middleware applied
    for _, route := range m.routes {
        h := route.handler
        // Apply middlewares in reverse order (last wraps first)
        for i := len(m.middlewares) - 1; i >= 0; i-- {
            h = m.middlewares[i](h)
        }
        
        // Register for each syscall in the method
        for _, nr := range MethodToSyscalls[route.method] {
            reg.Register(nr, route.handlerWrapper(route))
        }
    }
    return reg
}

// Route-specific matching logic
func (m *Mux) handlerWrapper(route route) Handler {
    return HandlerFunc(func(ctx *Context) {
        matched := false
        if route.exact {
            matched = ctx.Path == route.path || route.path == "*"
        } else if route.pattern != nil {
            matched = route.pattern.MatchString(ctx.Path)
        } else {
            matched = strings.HasPrefix(ctx.Path, route.path)
        }
        if matched {
            route.handler.ServeSeccomp(ctx)
        }
    })
}
```

**Supervisor Loop** (simplified):

```go
func Supervise(lfd *os.File, config SupervisorConfig) error {
    for {
        req, err := recvNotification(lfd)
        if err != nil { /* handle errors */ }
        
        meta := config.SyscallRegistry.Lookup(req.Data.Nr)
        if meta == nil { continueNoop(); continue }
        
        path := readPath(req.Pid, req.Data.Args[meta.PathArgIdx])
        
        ctx := &Context{
            PID:     req.Pid,
            Syscall: req.Data.Nr,
            Method:  meta.Method,      // "OPEN", "STAT", etc.
            Path:    path,
            Args:    req.Data.Args,
            DirFD:   resolveDirFD(req.Data.Args, meta),
            Flags:   req.Data.Args[meta.FlagsArgIdx],
            Mask:    uint32(req.Data.Args[meta.MaskArgIdx]),
            Respond: &responder{lfd: lfd, id: req.Id},
        }
        
        // Dispatch to handler registry
        config.HandlerRegistry.Dispatch(ctx)
    }
}
```

---

## Migration from Current Code

| Current | New |
|---------|-----|
| `SupervisorConfig.OpenCallback` | `mux.Handle("OPEN", path, handler)` |
| `SupervisorConfig.StatCallback` | `mux.Handle("STAT", path, handler)` |
| Manual BPF in `InstallFilter()` | `mux.Build()` → `InstallFilter(filter)` |
| `syscallRegistry` map | `SyscallRegistry` + `MethodToSyscalls` |
| Individual handler funcs | `Handler` interface + built-ins |

### Backward Compatibility Shim

```go
// LegacyConfig adapts old SupervisorConfig to new Mux
func LegacyConfig(cfg *OldSupervisorConfig) *Mux {
    mux := NewMux()
    if cfg.OpenCallback != nil {
        mux.Handle("OPEN", "*", HandlerFunc(func(ctx *Context) {
            content, _ := cfg.OpenCallback(ctx.PID, ctx.Path)
            if content != nil {
                ctx.Respond.SpoofFile(*content)
            } else {
                ctx.Respond.Continue()
            }
        }))
    }
    // ... similar for StatCallback, etc.
    return mux
}
```

---

## Benefits of HTTP Mux Style

1. **Familiar mental model** - Developers know HTTP routing
2. **Composable** - Middleware, chaining, prefix/regex matching
3. **Declarative** - Routes describe *what* not *how*
4. **Testable** - Handlers are pure functions, easy to unit test
5. **Extensible** - Add methods, middleware, matchers without core changes
6. **Observable** - Built-in logging/metrics via middleware

---

## Open Questions

1. **Method naming**: `OPEN`/`STAT` vs `OPEN`/`OPENAT`/`OPENAT2`?
   - Current: Normalized (`OPEN` covers all three) - simpler
   - Alternative: Expose raw syscalls for fine-grained control

2. **Path resolution**: Should `Context.Path` be pre-resolved (cleaned)?
   - Yes - do it once in supervisor, not in every handler

3. **Concurrency**: Is `Mux` safe for concurrent registration?
   - Build() is called once at startup, then read-only - no sync needed

4. **Error handling**: Should `Handler.ServeSeccomp` return error?
   - No - use `Responder.Error()` for syscall errors, panic for bugs

5. **Performance**: Route matching on every syscall?
   - Can compile to decision tree or hash map in `Build()`