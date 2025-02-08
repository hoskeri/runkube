package sandbox

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/hoskeri/runkube/pkg/sandbox/procfs"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const sandboxCookie = "sandboxcookie-v1"

func writeCookie(c *Config) (string, error) {
	b := &bytes.Buffer{}
	enc := json.NewEncoder(b)
	if err := enc.Encode(c); err != nil {
		return "", err
	}

	return base64.URLEncoding.EncodeToString(b.Bytes()), nil
}

func readCookie(s string) (*Config, error) {
	c := &Config{}
	d, err := base64.URLEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, err
	}

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

// Config describes a sandbox
type Config struct {
	Version   string    `json:"version"`
	Hostname  string    `json:"hostname"`
	MachineID string    `json:"machineID"`
	IPAddress string    `json:"ipAddress"`
	Command   []string  `json:"command"`
	Env       []string  `json:"env"`
	Payload   []Payload `json:"payload"`
}

// New creates a sandbox
func New(c *Config) (*Sandbox, error) {
	return &Sandbox{config: c}, nil
}

// Sandbox is a configured sandbox
type Sandbox struct {
	config    *Config
	lastError error
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

func (s *Sandbox) localIPAddr() net.IP {
	return net.ParseIP(s.config.IPAddress)
}

type fsType struct {
	fstype string
	flags  int
	opts   string
}

var (
	fsProc    = &fsType{fstype: "proc", flags: unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME}
	fsCgroup2 = &fsType{fstype: "cgroup2", flags: unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME, opts: "memory_recursiveprot"}
	fsTmpfs   = &fsType{fstype: "tmpfs", flags: unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME}
	fsDevPts  = &fsType{fstype: "devpts", flags: unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME}
	fsDev     = &fsType{fstype: "tmpfs", flags: unix.MS_NOEXEC | unix.MS_NOSUID | unix.MS_RELATIME}
	fsBindRO  = &fsType{fstype: "", flags: unix.MS_NOSUID | unix.MS_BIND | unix.MS_RDONLY | unix.MS_RELATIME}
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

func (s *Sandbox) linkUp(l string) error {
	if s.lastError != nil {
		return s.lastError
	}
	err := func() error {
		link, err := netlink.LinkByName(l)
		if err != nil {
			return err
		}

		if err := netlink.LinkSetUp(link); err != nil {
			return err
		}

		if err := netlink.AddrAdd(link, &netlink.Addr{
			IPNet: &net.IPNet{
				IP:   s.localIPAddr(),
				Mask: s.localIPAddr().DefaultMask(),
			},
		}); err != nil {
			return err
		}

		return nil
	}()
	if err != nil {
		s.lastError = fmt.Errorf("linkUp %q: %w", l, err)
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

func (s *Sandbox) fakeProcfs() error {
	f := fsProc

	if s.lastError != nil {
		return fmt.Errorf("previous error %w", s.lastError)
	}

	s.lastError = func() error {
		realProc := filepath.Join(s.rootDir(), "/.real/proc")
		if err := os.MkdirAll(realProc, 0755); err != nil {
			return err
		}

		if err := unix.Mount("proc", realProc, f.fstype, uintptr(f.flags), f.opts); err != nil {
			return fmt.Errorf("mount fstype=%q, src=%q, dest=%q: error: %w", f.fstype, "proc", realProc, err)
		}

		targetPath := filepath.Join(s.rootDir(), "/proc")
		if err := os.MkdirAll(targetPath, 0755); err != nil {
			return err
		}

		ps, err := procfs.Mount(targetPath, &procfs.Options{
			RealProcPath: realProc,
		})

		if err := ps.WaitMount(); err != nil {
			return err
		}

		return err
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

// TODO follow SD_LISTEN_FDS convention.
const fdNameProxy = "fd_name_proxy"

func passFD(via string, f *os.File) error {
	var viaFd int
	if _, err := fmt.Sscanf(os.Getenv(via), "%d", &viaFd); err != nil {
		return fmt.Errorf("passFd.Scanf %q: %w", via, err)
	}

	fdf, err := net.FileConn(os.NewFile(uintptr(viaFd), ""))
	if err != nil {
		return err
	}
	uc := fdf.(*net.UnixConn)
	oob := unix.UnixRights(int(f.Fd()))
	_, _, err = uc.WriteMsgUnix(nil, oob, nil)
	if err != nil {
		return err
	}
	return uc.Close()
}

func (s *Sandbox) startProxy() error {
	pl, err := net.Listen("tcp", ":8443")
	if err != nil {
		return err
	}
	defer pl.Close()

	c, err := pl.(*net.TCPListener).File()
	if err != nil {
		return err
	}
	defer c.Close()

	return passFD(fdNameProxy, c)
}

func (s *Sandbox) run() error {
	if err := s.startProxy(); err != nil {
		return err
	}

	if err := unix.Mount("tmpfs", s.rootDir(), "tmpfs", 0, ""); err != nil {
		return fmt.Errorf("mount tmpfs to %q: %w", s.rootDir(), err)
	}

	s.setHostname()
	s.mount(fsProc, "proc", "proc")
	s.fakeProcfs()
	s.mount(fsCgroup2, "cgroup2", "sys/fs/cgroup")
	s.mount(fsDev, "dev", "dev")
	s.mount(fsDevPts, "devpts", "dev/pts")
	s.mount(fsTmpfs, "tmpfs", "tmp")
	for _, p := range s.config.Payload {
		switch p.Source.Type {
		case "bind":
			s.bind(fsBindRO, p.Target, p.Source.Value)
		case "write":
			s.write(p.Target, p.Source.Value, p.FileMode)
		}
	}

	s.write("/etc/passwd", "", 0755)
	s.write("/etc/group", "", 0755)
	s.write("/etc/hostname", s.hostname(), 0755)
	s.write("/etc/machine-id", s.machineID(), 0755)
	s.write("/etc/resolv.conf", "nameserver 127.0.0.1", 0755)
	s.write("/etc/os-release", `ID=sandbox\nNAME=sandbox\nVERSION=1`, 0755)

	s.linkUp("lo")
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

func (s *Sandbox) sandboxProxyListener(fd int) error {
	c, err := net.FileConn(os.NewFile(uintptr(fd), ""))
	if err != nil {
		return err
	}
	unixConn := c.(*net.UnixConn)
	oob := make([]byte, unix.CmsgSpace(8))
	_, _, _, _, err = unixConn.ReadMsgUnix(nil, oob)
	if err != nil {
		return err
	}

	msg, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return err
	}

	fds, err := unix.ParseUnixRights(&msg[0])
	if err != nil {
		return err
	}

	l, err := net.FileListener(os.NewFile(uintptr(fds[0]), ""))
	if err != nil {
		return err
	}

	// TODO move egress inputs to config.
	egress, err := NewForwarder(SingleHost("[::1]:6443"))
	if err != nil {
		return err
	}

	return http.Serve(l, egress)
}

// Run bootstraps and runs a sandbox
func (s *Sandbox) Run() error {
	ck, err := writeCookie(s.config)
	if err != nil {
		return err
	}

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("unix.Socketpair: %w", err)
	}

	go func() {
		if err := s.sandboxProxyListener(fds[0]); err != nil {
			slog.Error("sandboxProxyListener", "err", err)
			os.Exit(1)
		}
	}()

	proc, err := os.StartProcess(
		"/proc/self/exe",
		[]string{sandboxCookie, ck},
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
				os.NewFile(uintptr(fds[1]), ""),
			},
			Env: []string{
				fmt.Sprintf("%s=%d", fdNameProxy, fds[1]),
			},
		})
	if err != nil {
		return fmt.Errorf("sandbox.Run %w", err)
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
	if len(args) != 2 {
		return nil
	}

	if args[0] != sandboxCookie {
		slog.Debug("sandbox.Init found no cookie, nothing to do")
		return nil
	}

	c, err := readCookie(args[1])
	if err != nil {
		return err
	}

	u := &Sandbox{config: c}
	return u.run()
}

// MustInit is Init, except it exits instead of returning an error
func MustInit() {
	if err := Init(os.Args...); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox.MustInit error %#v\n", err)
		os.Exit(2)
	}
}
