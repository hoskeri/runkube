package filter

import ()

// FilterBuilder constructs BPF programs programmatically
type FilterBuilder struct {
	registry *Registry
	syscalls []int32
}

func NewFilterBuilder(r *Registry) *FilterBuilder {
	return &FilterBuilder{registry: r}
}

func (b *FilterBuilder) Intercept(syscalls ...int32) *FilterBuilder {
	b.syscalls = append(b.syscalls, syscalls...)
	return b
}

func (b *FilterBuilder) InterceptMethod(methods ...string) *FilterBuilder {
	for _, method := range methods {
		b.syscalls = append(b.syscalls, b.registry.SyscallsForMethod(method)...)
	}
	return b
}

func (b *FilterBuilder) InterceptAll() *FilterBuilder {
	b.syscalls = append(b.syscalls, b.registry.AllInterceptedSyscalls()...)
	return b
}
