package seccomp

import (
	"io"

	"golang.org/x/sys/unix"

	"github.com/hoskeri/runkube/pkg/sandbox/seccomp/internal"
)

// SpoofFile returns a handler that serves a file from memory
func SpoofFile(content string) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		memfd, err := unix.MemfdCreate("spoof", unix.MFD_CLOEXEC)
		if err != nil {
			ctx.Respond.Error(unix.EIO)
			return
		}
		_, _ = unix.Write(memfd, []byte(content))
		_, _ = unix.Seek(memfd, 0, io.SeekStart)
		ctx.Respond.SpoofFD(memfd)
		unix.Close(memfd) // Safe: kernel holds reference via SECCOMP_ADDFD
	})
}

// SpoofFileFunc returns handler with dynamic content
func SpoofFileFunc(fn func(path string) (string, error)) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		content, err := fn(ctx.Path)
		if err != nil {
			ctx.Respond.Error(unix.EIO)
			return
		}
		if content == "" {
			ctx.Respond.Continue()
			return
		}
		memfd, err := unix.MemfdCreate("spoof", unix.MFD_CLOEXEC)
		if err != nil {
			ctx.Respond.Error(unix.EIO)
			return
		}
		_, _ = unix.Write(memfd, []byte(content))
		_, _ = unix.Seek(memfd, 0, io.SeekStart)
		ctx.Respond.SpoofFD(memfd)
		unix.Close(memfd)
	})
}

// SpoofStat returns handler that fakes stat/statx/statfs
func SpoofStat(stat any) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		var addr uint64
		switch ctx.Method {
		case "STAT":
			addr = ctx.Args[2] // newfstatat: arg[2] is stat buffer
		case "STATX":
			addr = ctx.Args[4] // statx: arg[4] is statx buffer
		case "STATFS":
			addr = ctx.Args[1] // statfs: arg[1] is statfs buffer
		default:
			ctx.Respond.Continue()
			return
		}
		ctx.Respond.SpoofStat(addr, stat)
	})
}

// SpoofStatFunc returns handler with dynamic stat
func SpoofStatFunc(fn func(path string) (any, error)) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		stat, err := fn(ctx.Path)
		if err != nil {
			ctx.Respond.Error(unix.EIO)
			return
		}
		if stat == nil {
			ctx.Respond.Continue()
			return
		}
		var addr uint64
		switch ctx.Method {
		case "STAT":
			addr = ctx.Args[2]
		case "STATX":
			addr = ctx.Args[4]
		case "STATFS":
			addr = ctx.Args[1]
		default:
			ctx.Respond.Continue()
			return
		}
		ctx.Respond.SpoofStat(addr, stat)
	})
}

// SpoofReadlink returns handler that fakes readlink
func SpoofReadlink(target string) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		var bufAddr, bufSize uint64
		if ctx.Syscall == unix.SYS_READLINK {
			bufAddr = ctx.Args[1]
			bufSize = ctx.Args[2]
		} else { // READLINKAT
			bufAddr = ctx.Args[2]
			bufSize = ctx.Args[3]
		}
		ctx.Respond.SpoofReadlink(bufAddr, target, bufSize)
	})
}

// SpoofUname returns handler that fakes uname(2) with a static result
func SpoofUname(uts *unix.Utsname) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		ctx.Respond.SpoofUname(ctx.Args[0], uts)
	})
}

// SpoofUnameFunc returns handler with dynamic uname data
func SpoofUnameFunc(fn func() (*unix.Utsname, error)) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		uts, err := fn()
		if err != nil {
			ctx.Respond.Error(unix.EIO)
			return
		}
		if uts == nil {
			ctx.Respond.Continue()
			return
		}
		ctx.Respond.SpoofUname(ctx.Args[0], uts)
	})
}

// PassThrough continues natively (explicit no-op)
func PassThrough() internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		ctx.Respond.Continue()
	})
}

// Reject returns error (like 404)
func Reject(errno unix.Errno) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		ctx.Respond.Error(errno)
	})
}

// Chain combines multiple handlers (first to handle wins)
// A handler "handles" if it calls any Responder method except Continue()
func Chain(handlers ...internal.Handler) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		for _, h := range handlers {
			wrapped := &internal.HandledResponder{Responder: ctx.Respond}
			ctx.Respond = wrapped
			h.ServeSeccomp(ctx)
			if wrapped.Handled {
				return
			}
		}
	})
}

// MapHandler creates a handler from a path->handler map (exact matches)
func MapHandler(m map[string]internal.Handler) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		if h, ok := m[ctx.Path]; ok {
			h.ServeSeccomp(ctx)
		} else {
			ctx.Respond.Continue()
		}
	})
}

// PrefixMapHandler creates a handler from a prefix->handler map (longest prefix wins)
func PrefixMapHandler(m map[string]internal.Handler) internal.Handler {
	return internal.HandlerFunc(func(ctx *internal.Context) {
		var bestMatch string
		var bestHandler internal.Handler
		for prefix, h := range m {
			if len(prefix) > len(bestMatch) && len(ctx.Path) >= len(prefix) && ctx.Path[:len(prefix)] == prefix {
				bestMatch = prefix
				bestHandler = h
			}
		}
		if bestHandler != nil {
			bestHandler.ServeSeccomp(ctx)
		} else {
			ctx.Respond.Continue()
		}
	})
}
