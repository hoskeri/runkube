package filter

import (
	"errors"

	"golang.org/x/sys/unix"
)

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

func (b *FilterBuilder) Build() ([]unix.SockFilter, error) {
	if len(b.syscalls) == 0 {
		return nil, errors.New("no syscalls to intercept")
	}

	// Deduplicate
	seen := make(map[int32]bool)
	unique := make([]int32, 0, len(b.syscalls))
	for _, nr := range b.syscalls {
		if !seen[nr] {
			seen[nr] = true
			unique = append(unique, nr)
		}
	}
	b.syscalls = unique

	var filter []unix.SockFilter

	// Load syscall number (offset 0 of seccomp_data)
	filter = append(filter, unix.SockFilter{
		Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0,
	})

	// Build jump chain for each syscall
	// We use a binary decision tree for efficiency with many syscalls
	if len(b.syscalls) <= 8 {
		// Linear chain for small numbers (like current implementation)
		for i, nr := range b.syscalls {
			jumpsRemaining := uint8(len(b.syscalls) - i - 1)
			filter = append(filter, unix.SockFilter{
				Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K,
				K:    uint32(nr),
				Jt:   jumpsRemaining,
				Jf:   0,
			})
		}
	} else {
		// Binary search tree for larger sets
		filter = append(filter, buildJumpTree(b.syscalls, 0, len(b.syscalls)-1)...)
	}

	// Match: USER_NOTIF
	filter = append(filter, unix.SockFilter{
		Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_USER_NOTIF,
	})

	// No match: ALLOW
	filter = append(filter, unix.SockFilter{
		Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW,
	})

	return filter, nil
}

// buildJumpTree builds a balanced binary decision tree
func buildJumpTree(syscalls []int32, lo, hi int) []unix.SockFilter {
	if lo > hi {
		return nil
	}
	if lo == hi {
		return []unix.SockFilter{
			{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(syscalls[lo]), Jt: 1, Jf: 0},
			{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_USER_NOTIF},
		}
	}

	mid := (lo + hi) / 2
	var filter []unix.SockFilter

	// Compare with mid value
	filter = append(filter, unix.SockFilter{
		Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(syscalls[mid]), Jt: 1, Jf: 0,
	})

	// If equal -> USER_NOTIF (handled by next instruction)
	filter = append(filter, unix.SockFilter{
		Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_USER_NOTIF,
	})

	// If less than mid -> left subtree
	// We need to load syscall number again for the subtree
	leftTree := buildJumpTree(syscalls, lo, mid-1)
	if len(leftTree) > 0 {
		// Insert load instruction before left tree
		leftTree = append([]unix.SockFilter{
			{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		}, leftTree...)
		filter = append(filter, leftTree...)
	}

	// If greater than mid -> right subtree
	rightTree := buildJumpTree(syscalls, mid+1, hi)
	if len(rightTree) > 0 {
		rightTree = append([]unix.SockFilter{
			{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		}, rightTree...)
		filter = append(filter, rightTree...)
	}

	return filter
}