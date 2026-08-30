package sandbox

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/hoskeri/runkube/pkg/randid"
	"github.com/hoskeri/runkube/pkg/sandbox/fdchan"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const sandboxCookie = "sandboxcookie-v1"

// setupReady is the child's Data in the single setup RPC response: it
// signals that the child has assembled the descriptors the parent needs and
// is continuing with the rest of the sandbox setup.
const setupReady = "setup-ok"

// encodeConfig serializes a Config for transfer as setup-RPC metadata.
func encodeConfig(c *Config) ([]byte, error) {
	return json.Marshal(c)
}

// decodeConfig parses a Config from setup-RPC metadata.
func decodeConfig(d []byte) (*Config, error) {
	c := &Config{}
	dec := json.NewDecoder(bytes.NewReader(d))
	dec.DisallowUnknownFields()
	if err := dec.Decode(c); err != nil {
		return nil, err
	}

	return c, nil
}

// PayloadSource is the source for a payload item
type PayloadSource struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Payload is a single file injected into a sandbox
type Payload struct {
	Target   string        `type:"target"`
	FileMode os.FileMode   `type:"fileMode"`
	Source   PayloadSource `type:"source"`
}

type Features struct {
	SyscallFilter bool `json:"syscallFilter"`
	DebugLog      bool `json:"debugLog"`
}

// Config describes a sandbox
type Config struct {
	Version   string    `json:"version"`
	Hostname  string    `json:"hostname"`
	MachineID string    `json:"machineID"`
	Command   []string  `json:"command"`
	Env       []string  `json:"env"`
	Payload   []Payload `json:"payload"`
	Features  Features  `json:"features"`
	// Egress is the upstream address the sandbox's egress CONNECT proxy
	// forwards to, e.g. "127.0.0.1:6443". When empty, the default
	// ("[::1]:6443") is used.
	Egress string `json:"egress,omitempty"`
	// IPAddress optionally overrides the IPv4 address of the sandbox's
	// default interface. When empty, the address is derived from the
	// machine id (randid.IPv4) together with the MAC and IPv6 addresses, so
	// the identity is predictable but not hardcoded.
	IPAddress string `json:"ipAddress"`
	// System overrides the emulated hardware/resources presented through
	// /proc, /sys and uname(2). When nil, DefaultSystem is used.
	System *System `json:"system,omitempty"`
}

// New creates a sandbox
func New(c *Config) (*Sandbox, error) {
	return &Sandbox{config: c}, nil
}

// Sandbox is a configured sandbox
type Sandbox struct {
	config    *Config
	sys       *System
	lastError error
	// rootfs is the parent-side handle to the container rootfs, transferred
	// by the child in the setup response. nil until Run() has exchanged it.
	rootfs *Rootfs
}

// system returns the emulated System model for this sandbox: the configured
// model when present, otherwise DefaultSystem. Resolved once and memoized so
// identity (boot id, boot time) stays stable for the sandbox.
func (s *Sandbox) system() *System {
	if s.sys == nil {
		m := s.config.System
		if m == nil {
			m = DefaultSystem()
		}
		s.sys = m.Resolve()
	}
	return s.sys
}

// Rootfs returns the parent's handle to the container rootfs, transferred by
// the child during setup. It lets the system model create and update files
// inside the running container (docs/TODO.md, "Dynamic file injection").
// nil until Run() has completed the setup exchange.
func (s *Sandbox) Rootfs() *Rootfs {
	return s.rootfs
}

// InjectFiles writes model-rendered files into the container rootfs at
// runtime, replacing the boot-time copies. It is the public runtime entry
// point for the system model, e.g. re-rendering SysfsDevices() after a CPU or
// memory change; granular files are written with s.Rootfs().Write.
func (s *Sandbox) InjectFiles(files []SysfsFile) error {
	if s.rootfs == nil {
		return fmt.Errorf("InjectFiles: no rootfs handle, sandbox has not completed setup")
	}
	for _, f := range files {
		if err := s.rootfs.Write(f.Path, []byte(f.Content), 0444); err != nil {
			return err
		}
	}
	return nil
}

// injectBootFiles writes bootFiles into the container rootfs through the
// transferred rootfs fd. Content comes from the same bootFiles() list the
// child materializes, so the copies cannot diverge; this parent-side
// injection is the handle the system model keeps for runtime updates.
func (s *Sandbox) injectBootFiles() error {
	if s.rootfs == nil {
		return fmt.Errorf("injectBootFiles: no rootfs handle")
	}
	for _, f := range s.bootFiles() {
		if err := s.rootfs.Write(f.path, f.content, f.mode); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sandbox) hostname() string {
	return s.config.Hostname
}

func (s *Sandbox) machineID() string {
	return s.config.MachineID
}

func (s *Sandbox) rootDir() string {
	return "/tmp"
}

type fsType struct {
	fstype string
	flags  int
	opts   string
}

var (
	fsCgroup2 = &fsType{
		fstype: "cgroup2",
		flags:  unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME,
		opts:   "memory_recursiveprot",
	}
	fsProc   = &fsType{fstype: "proc", flags: unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME}
	fsTmpfs  = &fsType{fstype: "tmpfs", flags: unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME}
	fsDevPts = &fsType{fstype: "devpts", flags: unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME}
	fsDev    = &fsType{fstype: "tmpfs", flags: unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME}
	fsBindRO = &fsType{fstype: "", flags: unix.MS_NOSUID | unix.MS_BIND | unix.MS_RDONLY | unix.MS_RELATIME}
)

func (s *Sandbox) setHostname() error {
	if s.lastError != nil {
		return s.lastError
	}
	err := unix.Sethostname([]byte(s.hostname()))
	if err != nil {
		s.lastError = fmt.Errorf("unix.Sethostname: %w", err)
	}

	return s.lastError
}

// defaultInterface is the name of the sandbox's emulated default interface.
const defaultInterface = "eth0"

// setupNetwork brings up the sandbox's network namespace. The loopback
// interface gets its kernel-assigned 127.0.0.1/8 and ::1/128 addresses, and
// the default interface (eth0, a dummy device) is created with a MAC, IPv4
// and IPv6 address derived deterministically from the machine id
// (randid.MAC/IPv4/IPv6), so the identity is predictable across restarts but
// not hardcoded. Config.IPAddress, when set, overrides the derived IPv4.
func (s *Sandbox) setupNetwork() error {
	if s.lastError != nil {
		return s.lastError
	}

	s.lastError = func() error {
		lo, err := netlink.LinkByName("lo")
		if err != nil {
			return fmt.Errorf("link %q: %w", "lo", err)
		}
		if err := netlink.LinkSetUp(lo); err != nil {
			return fmt.Errorf("link up %q: %w", "lo", err)
		}

		mac := randid.MAC(s.machineID())
		ipv4 := randid.IPv4(s.machineID())
		if ip := net.ParseIP(s.config.IPAddress); ip != nil {
			ipv4 = ip
		}
		ipv6 := randid.IPv6(s.machineID())

		attrs := netlink.NewLinkAttrs()
		attrs.Name = defaultInterface
		attrs.HardwareAddr = mac
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: attrs}); err != nil {
			return fmt.Errorf("create %q: %w", defaultInterface, err)
		}
		link, err := netlink.LinkByName(defaultInterface)
		if err != nil {
			return fmt.Errorf("link %q: %w", defaultInterface, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("link up %q: %w", defaultInterface, err)
		}

		for _, a := range []*netlink.Addr{
			{IPNet: &net.IPNet{IP: ipv4, Mask: net.CIDRMask(24, 32)}},
			{IPNet: &net.IPNet{IP: ipv6, Mask: net.CIDRMask(64, 128)}},
		} {
			if err := netlink.AddrAdd(link, a); err != nil {
				return fmt.Errorf("addr add %v: %w", a.IPNet, err)
			}
		}
		return nil
	}()

	if s.lastError != nil {
		s.lastError = fmt.Errorf("setupNetwork: %w", s.lastError)
	}
	return s.lastError
}

func (s *Sandbox) pivot() error {
	if s.lastError != nil {
		return fmt.Errorf("previous error %w", s.lastError)
	}

	s.lastError = func() error {
		if err := os.Chdir(s.rootDir()); err != nil {
			return fmt.Errorf("chdir %q: %w", s.rootDir(), err)
		}

		if err := unix.PivotRoot(".", "."); err != nil {
			return fmt.Errorf("pivot_root: %w", err)
		}

		if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
			return fmt.Errorf("unmount put_old: %w", err)
		}

		if err := os.Chdir("/"); err != nil {
			return fmt.Errorf("chdir /: %w", err)
		}

		return nil
	}()

	return s.lastError
}

func (s *Sandbox) write(dest string, c string, mode os.FileMode) error {
	if s.lastError != nil {
		return s.lastError
	}

	s.lastError = func() error {
		p := filepath.Join(s.rootDir(), dest)
		d := filepath.Dir(p)
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}

		m := mode
		if m.Perm() == 0 {
			m = 0755
		}

		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|unix.O_CLOEXEC, m)
		if err != nil {
			return err
		}
		defer f.Close()
		if !strings.HasSuffix(c, "\n") {
			c = c + "\n"
		}
		n, err := f.WriteString(c)
		if err != nil {
			return err
		}
		if w := len(c); n != w {
			return fmt.Errorf("write(%q) write(%d) != %d", dest, n, w)
		}
		return nil
	}()

	return s.lastError
}

// rootfsFile is one regular file the sandbox config injects into the
// container rootfs.
type rootfsFile struct {
	path    string
	content []byte
	mode    os.FileMode
}

// bootFiles renders the config-derived regular files injected into the
// container rootfs: the model-rendered /sys/devices/system tree followed by
// write-type payloads (which may override a sysfs file). Both the child's
// boot materialization and the parent's rootfs-fd injection render from this
// single list, so the two copies are byte-identical by construction. bind
// payloads are mounts, not files, and stay child-side.
func (s *Sandbox) bootFiles() []rootfsFile {
	var out []rootfsFile
	for _, f := range s.system().SysfsDevices() {
		out = append(out, rootfsFile{path: f.Path, content: []byte(f.Content), mode: 0444})
	}
	for _, p := range s.config.Payload {
		if p.Source.Type != "write" {
			continue
		}
		content := p.Source.Value
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		mode := p.FileMode
		if mode.Perm() == 0 {
			mode = 0755
		}
		out = append(out, rootfsFile{path: p.Target, content: []byte(content), mode: mode})
	}
	return out
}

// materializeBootFiles writes bootFiles into the rootfs during child setup,
// resolved against the rootfs handle rather than host paths. The parent
// injects the same list through the transferred rootfs fd as soon as the
// setup response is received (see injectBootFiles); this child-side copy runs
// in the boot path itself and is the same atomic-write mechanism, so
// container startup never depends on parent/child scheduling after the
// handshake and the two copies can never be torn or diverge.
func (s *Sandbox) materializeBootFiles(rootfs *Rootfs) error {
	if s.lastError != nil {
		return s.lastError
	}
	s.lastError = func() error {
		for _, f := range s.bootFiles() {
			if err := rootfs.Write(f.path, f.content, f.mode); err != nil {
				return err
			}
		}
		return nil
	}()
	return s.lastError
}

func (s *Sandbox) mount(f *fsType, src, dest string) error {
	if s.lastError != nil {
		return fmt.Errorf("previous error %w", s.lastError)
	}

	s.lastError = func() error {
		actualDest := filepath.Join(s.rootDir(), dest)
		if err := os.MkdirAll(actualDest, 0755); err != nil {
			return err
		}

		if err := unix.Mount(src, actualDest, f.fstype, uintptr(f.flags), f.opts); err != nil {
			return fmt.Errorf("mount fstype=%q, src=%q, dest=%q: error: %w", f.fstype, src, actualDest, err)
		}

		return nil
	}()
	return s.lastError
}

func (s *Sandbox) bind(f *fsType, target, source string) error {
	if s.lastError != nil {
		return fmt.Errorf("previous error %w", s.lastError)
	}

	s.lastError = func() error {
		sfi, err := os.Stat(source)
		if err != nil {
			return err
		}

		targetPath := filepath.Join(s.rootDir(), target)
		if sfi.IsDir() {
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return err
			}
		} else {
			if err := s.write(target, "", 0755); err != nil {
				return err
			}
		}

		if err := unix.Mount(source, targetPath, "", uintptr(f.flags), f.opts); err != nil {
			return fmt.Errorf("mount fstype=%q, target=%q, source=%q: error: %w", f.fstype, target, source, err)
		}

		return nil
	}()
	return s.lastError
}

func (s *Sandbox) debugShell() error {
	if err := unix.Exec("/usr/bin/busybox", []string{"/usr/bin/busybox", "sh"}, []string{}); err != nil {
		return fmt.Errorf("unix.Exec: %w", err)
	}

	return nil
}

// makeProxyListener creates the TCP listener the sandbox command uses as its
// HTTPS proxy (127.0.0.1:8443) and returns its detached file handle for
// transfer to the parent in the setup response. The returned file stays
// open; the caller transfers it and then closes it.
func (s *Sandbox) makeProxyListener() (*os.File, error) {
	pl, err := net.Listen("tcp", ":8443")
	if err != nil {
		return nil, err
	}
	defer pl.Close()

	return pl.(*net.TCPListener).File()
}

// run is the sandboxed child entry point. All parent/child coordination
// happens in a single "setup" RPC: the parent sends metadata (the sandbox
// Config) plus any parent-side file descriptors, and the child replies with
// the descriptors the parent needs to run the sandbox (the proxy listener,
// an fd on the container rootfs for file injection, and when enabled the
// seccomp user-notify listener). Everything after that exchange is one-way:
// the child finishes booting and execs the sandboxed command.
func (s *Sandbox) run() error {
	runtime.LockOSThread()

	child, err := fdchan.NewChild()
	if err != nil {
		return err
	}

	slog.Debug("fdchan child", "child", child)

	req, responder, err := child.HandleRequest()
	if err != nil {
		return err
	}

	c, err := decodeConfig(req.Data)
	if err != nil {
		return fmt.Errorf("setup request: %w", err)
	}
	s.config = c

	if c.Features.DebugLog {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	if err := unix.Mount("tmpfs", s.rootDir(), "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount tmpfs to %q: %w", s.rootDir(), err)
	}

	// The rootfs fd handed to the parent in the setup response is its handle
	// to the container filesystem: the system model injects and updates files
	// through it (docs/TODO.md, "Dynamic file injection"). Opened with the
	// rest of the descriptor setup, before the seccomp filter is installed,
	// and closed when run() returns (O_CLOEXEC keeps it out of the exec'd
	// command). The child uses the same handle locally for its own boot-file
	// materialization, so both copies are written by the same mechanism.
	rootfsFD, err := unix.Open(s.rootDir(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open rootfs dir: %w", err)
	}
	rootfsF := os.NewFile(uintptr(rootfsFD), "rootfs")
	defer rootfsF.Close()
	rootfs := NewRootfs(rootfsF)

	s.mount(fsDev, "dev", "dev")
	s.bind(fsBindRO, "/dev/null", "/dev/null")
	if s.lastError != nil {
		return s.lastError
	}

	// Reply to the single setup RPC with the descriptors the parent needs.
	// The proxy listener is created before the seccomp filter is installed:
	// net.Listen probes uname(2) (MPTCP support detection), which the
	// DefaultFilter traps, and the parent cannot answer notifications until
	// it receives this response.
	resp := fdchan.Message{Data: []byte(setupReady)}

	lfd, err := s.makeProxyListener()
	if err != nil {
		return err
	}
	if err := resp.AddDescriptor(lfd, "proxy-listener", "tcp listener"); err != nil {
		return err
	}

	if err := resp.AddDescriptor(rootfsF, "rootfs-fd", "container rootfs directory (file injection)"); err != nil {
		return err
	}

	if s.config.Features.SyscallFilter {
		fd, err := s.installSyscallFilter()
		if err != nil {
			return err
		}
		if err := resp.AddDescriptor(os.NewFile(uintptr(fd), "seccomp-notify"), "seccomp-fd", "seccomp user-notify listener"); err != nil {
			return err
		}
	}

	if err := responder(resp); err != nil {
		return err
	}
	lfd.Close()

	s.setHostname()
	s.mount(fsProc, "proc", "proc")
	s.mount(fsCgroup2, "cgroup2", "sys/fs/cgroup")
	s.mount(fsDevPts, "devpts", "dev/pts")
	s.mount(fsTmpfs, "tmpfs", "tmp")

	s.materializeBootFiles(rootfs)

	// bind payloads are real mounts and can only be performed from inside the
	// sandbox mount namespace; write payloads are covered by
	// materializeBootFiles.
	for _, p := range s.config.Payload {
		if p.Source.Type == "bind" {
			s.bind(fsBindRO, p.Target, p.Source.Value)
		}
	}

	s.write("/etc/passwd", "", 0755)
	s.write("/etc/group", "", 0755)
	s.write("/etc/hostname", s.hostname(), 0755)
	s.write("/etc/machine-id", s.machineID(), 0755)
	s.write("/etc/resolv.conf", "nameserver 127.0.0.1", 0755)
	s.write("/etc/os-release", `ID=sandbox\nNAME=sandbox\nVERSION=1`, 0755)

	s.setupNetwork()
	s.pivot()

	if s.lastError != nil {
		return s.lastError
	}

	slog.Debug("u.run", "pid", os.Getpid())
	if len(s.config.Command) == 0 {
		return fmt.Errorf("config.Command not set")
	}

	v := "127.0.0.1:8443"
	pe := []string{
		fmt.Sprintf("HTTPS_PROXY=%s", v),
	}

	if err := unix.Exec(s.config.Command[0], s.config.Command, append(s.config.Env, pe...)); err != nil {
		return fmt.Errorf("unix.Exec: %w", err)
	}
	return nil
}

// startEgress serves the sandbox's egress CONNECT proxy on the listener fd
// the child created and returned in the setup response.
func (s *Sandbox) startEgress(lfd *os.File) error {
	l, err := net.FileListener(lfd)
	if err != nil {
		return err
	}

	go func() {
		egress, err := NewForwarder(SingleHost(s.egressTarget()))
		if err != nil {
			return
		}

		http.Serve(l, egress)
	}()

	return nil
}

// egressTarget returns the upstream address the egress forwarder proxies to:
// Config.Egress when set, otherwise the default API server address.
func (s *Sandbox) egressTarget() string {
	if t := s.config.Egress; t != "" {
		return t
	}
	return "[::1]:6443"
}

// Run bootstraps and runs a sandbox
func (s *Sandbox) Run() error {
	if s.config.Features.DebugLog {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	ck, err := encodeConfig(s.config)
	if err != nil {
		return err
	}

	control, err := fdchan.NewParent()
	if err != nil {
		return err
	}

	childFD := control.ChildFD()

	proc, err := os.StartProcess(
		"/proc/self/exe",
		[]string{sandboxCookie}, // marker only; the Config travels in the setup RPC
		&os.ProcAttr{
			Sys: &syscall.SysProcAttr{
				Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWPID,
				Unshareflags: uintptr(
					unix.CLONE_NEWUTS | unix.CLONE_NEWNS | unix.CLONE_NEWIPC |
						unix.CLONE_NEWCGROUP | unix.CLONE_NEWTIME | unix.CLONE_NEWNET,
				),
				UidMappings: []syscall.SysProcIDMap{
					{ContainerID: 0, HostID: os.Getuid(), Size: 1},
				},
				GidMappings: []syscall.SysProcIDMap{
					{ContainerID: 0, HostID: os.Getgid(), Size: 1},
				},
				GidMappingsEnableSetgroups: false,
				Pdeathsig:                  syscall.SIGKILL,
			},
			Files: []*os.File{
				os.Stdin,
				os.Stdout,
				os.Stderr,
				os.NewFile(uintptr(childFD), ""),
			},
			Env: []string{
				fmt.Sprintf("%s=%d", fdchan.EnvVarKey, childFD),
			},
		})
	if err != nil {
		return fmt.Errorf("sandbox.Run %w", err)
	}

	// The single setup roundtrip: metadata (the Config) plus any parent-side
	// descriptors go down to the child; the child's metadata and descriptors
	// (rootfs fd, proxy listener, seccomp notify listener) come back up.
	resp, err := control.Request(fdchan.Message{Data: ck})
	if err != nil {
		return err
	}
	if string(resp.Data) != setupReady {
		return fmt.Errorf("unexpected setup response, got %#v", resp.Data)
	}

	if s.config.Features.SyscallFilter {
		seccompFD, _, err := resp.GetDescriptor("seccomp-fd")
		if err != nil {
			return fmt.Errorf("seccomp-fd descriptor error %w", err)
		}
		if err := s.superviseSyscallFilter(seccompFD); err != nil {
			return fmt.Errorf("s.superviseSyscallFilter error %w", err)
		}
	}

	// The child transferred an fd on the container rootfs: the parent's
	// handle for injecting files into the running container. The model
	// writes the boot-time files (sysfs tree, write payloads) through it
	// here; the same handle stays available for runtime updates via
	// s.Rootfs()/s.InjectFiles.
	rootfsFile, _, err := resp.GetDescriptor("rootfs-fd")
	if err != nil {
		return fmt.Errorf("rootfs-fd descriptor error %w", err)
	}
	s.rootfs = NewRootfs(rootfsFile)
	defer s.rootfs.Close()
	if err := s.injectBootFiles(); err != nil {
		return fmt.Errorf("inject boot files into rootfs: %w", err)
	}

	lfd, _, err := resp.GetDescriptor("proxy-listener")
	if err != nil {
		return fmt.Errorf("proxy-listener descriptor error %w", err)
	}
	if err := s.startEgress(lfd); err != nil {
		return err
	}

	slog.Debug("sandbox.Run started", "pid", proc.Pid)
	st, err := proc.Wait()
	slog.Info("done waiting", "err", err, "st", st)
	return err
}

// Init executes the initial sandboxed process. Init must be called as early
// as possible during process start. The calling process is expected to exit
// if this function returns an error
func Init(args ...string) error {
	if len(args) != 1 || args[0] != sandboxCookie {
		slog.Debug("sandbox.Init found no cookie, nothing to do")
		return nil
	}

	// The Config arrives as metadata in the single setup RPC; run() parses it
	// before doing any setup.
	u := &Sandbox{}
	return u.run()
}

// MustInit is Init, except it exits instead of returning an error
func MustInit() {
	if err := Init(os.Args...); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox.MustInit error %s\n", err.Error())
		os.Exit(2)
	}
}
