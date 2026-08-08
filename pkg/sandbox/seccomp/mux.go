package seccomp

import (
	"regexp"
	"strings"

	"github.com/hoskeri/runkube/pkg/sandbox/seccomp/filter"
	"github.com/hoskeri/runkube/pkg/sandbox/seccomp/internal"
)

// route represents a single route in the mux
type route struct {
	method  string
	path    string
	pattern *regexp.Regexp
	handler internal.Handler
	exact   bool // true for exact match, false for prefix/pattern
}

// Mux routes syscall+path to handlers, like http.ServeMux
type Mux struct {
	routes      []route
	middlewares []internal.Middleware
	registry    *filter.Registry
}

func NewMux() *Mux {
	return &Mux{
		registry: filter.DefaultRegistry,
	}
}

// SetRegistry sets a custom syscall registry (for testing or extension)
func (m *Mux) SetRegistry(r *filter.Registry) *Mux {
	m.registry = r
	return m
}

// Handle registers a handler for exact syscall+path match
// method: "OPEN", "STAT", "STATX", "STATFS", "READLINK"
// path: exact path like "/proc/mounts" or "*" for catch-all
func (m *Mux) Handle(method, path string, h internal.Handler) *Mux {
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
func (m *Mux) HandlePrefix(method, prefix string, h internal.Handler) *Mux {
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
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
func (m *Mux) HandlePattern(method, pattern string, h internal.Handler) *Mux {
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
func (m *Mux) Use(mw internal.Middleware) *Mux {
	m.middlewares = append(m.middlewares, mw)
	return m
}

// SupervisorConfig holds the configuration for the supervisor loop
type SupervisorConfig = internal.SupervisorConfig

// Build compiles the mux into a Filter + SupervisorConfig
func (m *Mux) Build() (Filter, SupervisorConfig) {
	// 1. Collect unique syscalls from routes
	syscalls := m.collectSyscalls()

	// 2. Build BPF filter
	builder := filter.NewFilterBuilder(m.registry)
	builder.Intercept(syscalls...)
	bpfFilter, _ := builder.Build()

	// 3. Build handler registry with middleware chain
	handlerReg := m.buildHandlerRegistry()

	return Filter(bpfFilter), SupervisorConfig{
		HandlerRegistry: handlerReg,
		SyscallRegistry: m.registry,
	}
}

func (m *Mux) collectSyscalls() []int32 {
	var syscalls []int32
	seen := make(map[int32]bool)
	for _, r := range m.routes {
		for _, nr := range m.registry.SyscallsForMethod(r.method) {
			if !seen[nr] {
				seen[nr] = true
				syscalls = append(syscalls, nr)
			}
		}
	}
	return syscalls
}

func (m *Mux) buildHandlerRegistry() *internal.HandlerRegistry {
	reg := internal.NewHandlerRegistry()

	// Convert routes to handlers with middleware applied
	for _, r := range m.routes {
		h := r.handler

		// Apply middlewares in reverse order (last wraps first)
		for i := len(m.middlewares) - 1; i >= 0; i-- {
			h = m.middlewares[i](h)
		}

		// Wrap with route matching logic
		wrapped := m.wrapRouteMatcher(r, h)

		// Register for each syscall in the method
		for _, nr := range m.registry.SyscallsForMethod(r.method) {
			reg.Register(nr, wrapped)
		}
	}

	return reg
}

func (m *Mux) wrapRouteMatcher(r route, h internal.Handler) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		matched := false
		if r.exact {
			matched = ctx.Path == r.path || r.path == "*"
		} else if r.pattern != nil {
			matched = r.pattern.MatchString(ctx.Path)
		} else {
			matched = strings.HasPrefix(ctx.Path, r.path)
		}
		if matched {
			h.ServeSeccomp(ctx)
		}
	})
}

// DefaultMux returns a mux with common defaults
func DefaultMux() *Mux {
	return NewMux().Use(LoggingMiddleware())
}