package filter

import (
	"github.com/hoskeri/runkube/pkg/sandbox/seccomp/internal"
)

// Registry holds all syscall metadata
type Registry struct {
	*internal.RegistryImpl
}

func NewRegistry() *Registry {
	return &Registry{RegistryImpl: internal.NewRegistryImpl()}
}

// DefaultRegistry is the global registry with all standard syscalls
var DefaultRegistry = NewRegistry()
