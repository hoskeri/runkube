package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// System is the typed model of the emulated Linux hardware and resources a
// sandbox presents through procfs, sysfs and uname(2). The model is kept
// deliberately small: just enough knobs to render compatible data without
// pretending to model a real kernel. Values are carried in the sandbox
// Config (JSON) so callers can size the emulated node.
type System struct {
	Arch      string    `json:"arch,omitempty"` // uname machine, e.g. "x86_64"
	CPU       CPU       `json:"cpu"`
	Memory    Memory    `json:"memory"`
	HugePages HugePages `json:"hugePages,omitempty"`
	Kernel    Kernel    `json:"kernel,omitempty"`

	// booted is the emulated boot time, set when the model is resolved.
	// Not serialized: each sandbox instance boots independently.
	booted time.Time
}

// CPU describes the emulated processor topology. Threads is the total number
// of logical processors (SMT threads), Cores the total number of physical
// cores; Sockets is optional and defaults to one package.
type CPU struct {
	Vendor    string   `json:"vendor,omitempty"`    // e.g. "GenuineIntel"
	Model     string   `json:"model,omitempty"`     // model name line in /proc/cpuinfo
	Family    int      `json:"family,omitempty"`    // "cpu family"
	ModelNo   int      `json:"modelNo,omitempty"`   // "model"
	Sockets   int      `json:"sockets,omitempty"`   // physical packages
	Cores     int      `json:"cores"`               // physical cores
	Threads   int      `json:"threads"`             // logical processors
	MHz       int      `json:"mhz,omitempty"`       // nominal clock
	MaxMHzKHz int      `json:"maxMhzKhz,omitempty"` // cpufreq cpuinfo_max_freq, kHz
	CacheKB   int      `json:"cacheKb,omitempty"`   // L3 cache size
	Flags     []string `json:"flags,omitempty"`     // cpuid feature flags
}

// Memory describes RAM and swap, in bytes.
type Memory struct {
	Total     int64 `json:"total"`
	Available int64 `json:"available"`
	SwapTotal int64 `json:"swapTotal,omitempty"`
	SwapFree  int64 `json:"swapFree,omitempty"`
}

// HugePages describes the hugetlb pool surfaced via /proc/meminfo.
type HugePages struct {
	Total  int64 `json:"total,omitempty"`  // number of huge pages
	Free   int64 `json:"free,omitempty"`   // number currently free
	SizeKB int64 `json:"sizeKb,omitempty"` // page size in kB, e.g. 2048
}

// Kernel describes the emulated kernel identity reported by uname(2),
// /proc/version and /proc/sys/kernel/*.
type Kernel struct {
	Release   string `json:"release,omitempty"`   // uname -r, e.g. "6.6.26-sandbox"
	Version   string `json:"version,omitempty"`   // uname -v and sys/kernel/version, e.g. "#1 SMP Fri Jan  1 00:00:00 UTC 2024"
	Builder   string `json:"builder,omitempty"`   // /proc/version build host
	Toolchain string `json:"toolchain,omitempty"` // /proc/version compiler string
	BootID    string `json:"bootId,omitempty"`    // /proc/sys/kernel/{,random/}boot_id
	Domain    string `json:"domain,omitempty"`    // uname domainname and sys/kernel/domainname
}

// ---------------------------------------------------------------------------
// Defaults

var defaultCpuFlags = strings.Fields(`
fpu vme de pse tsc msr pae mce cx8 apic sep mtrr pge mca cmov pat pse36 clflush
mmx fxsr sse sse2 ss syscall nx pdpe1gb rdtscp lm constant_tsc rep_good nopl
xtopology cpuid tsc_known_freq pni pclmulqdq ssse3 fma cx16 pcid sse4_1 sse4_2
x2apic movbe popcnt aes xsave avx f16c rdrand hypervisor lahf_lm abm fsgsbase
bmi1 avx2 smep bmi2 erms invpcid avx512f avx512dq rdseed adx smap avx512cd
sha_ni avx512bw avx512vl xsaveopt xsavec xgetbv1 arat umip pku ospke
avx512_vbmi2 gfni vaes vpclmulqdq avx512_vnni avx512_bitalg avx512_vpopcntdq
rdpid fsrm`)

const (
	defaultRelease   = "6.6.26-sandbox"
	defaultVersion   = "#1 SMP Fri Jan  1 00:00:00 UTC 2024"
	defaultDomain    = "localdomain"
	defaultModel     = "Intel(R) Xeon(R) CPU @ 2.50GHz"
	defaultVendor    = "GenuineIntel"
	defaultMemory    = int64(8 << 30)
	defaultAvailable = defaultMemory * 3 / 4
	defaultSwap      = int64(2 << 30)
)

// DefaultSystem returns a small, deterministic emulation: 4 CPUs / 8 GiB RAM,
// 2 GiB swap and a 2 MiB hugetlb pool.
func DefaultSystem() *System {
	return &System{
		Arch: archForGOARCH(runtime.GOARCH),
		CPU: CPU{
			Vendor: defaultVendor, Model: defaultModel,
			Family: 6, ModelNo: 158,
			Sockets: 16, Cores: 16, Threads: 16,
			MHz: 2500, MaxMHzKHz: 2500000, CacheKB: 16384,
		},
		Memory:    Memory{Total: defaultMemory, Available: defaultAvailable, SwapTotal: defaultSwap, SwapFree: defaultSwap},
		HugePages: HugePages{Total: 128, Free: 96, SizeKB: 2048},
		Kernel: Kernel{
			Release:   defaultRelease,
			Version:   defaultVersion,
			Builder:   "build@sandbox",
			Toolchain: "gcc (GCC) 12.2.0, GNU ld version 2.38",
			Domain:    defaultDomain,
		},
	}
}

// Resolve fills optional fields with defaults, generates a boot id and records
// the emulated boot time so /proc/uptime advances. It is safe to call on a
// shared/JSON-decoded model; the same instance keeps its generated identity.
func (s *System) Resolve() *System {
	if s == nil {
		return DefaultSystem().Resolve()
	}
	if s.Arch == "" {
		s.Arch = archForGOARCH(runtime.GOARCH)
	}
	s.CPU.applyDefaults()
	if s.Memory.Total <= 0 {
		s.Memory.Total = defaultMemory
	}
	if s.Memory.Available <= 0 || s.Memory.Available > s.Memory.Total {
		s.Memory.Available = s.Memory.Total
	}
	if s.HugePages.SizeKB <= 0 {
		s.HugePages.SizeKB = 2048
	}
	if s.Kernel.Release == "" {
		s.Kernel.Release = defaultRelease
	}
	if s.Kernel.Version == "" {
		s.Kernel.Version = defaultVersion
	}
	if s.Kernel.Domain == "" {
		s.Kernel.Domain = defaultDomain
	}
	if s.Kernel.BootID == "" {
		s.Kernel.BootID = randomUUID()
	}
	if s.booted.IsZero() {
		s.booted = time.Now()
	}
	return s
}

func (c *CPU) applyDefaults() {
	if c.Sockets <= 0 {
		c.Sockets = 1
	}
	if c.Cores <= 0 && c.Threads <= 0 {
		c.Cores, c.Threads = 1, 1
	}
	if c.Threads <= 0 {
		c.Threads = c.Cores * c.Sockets
	}
	if c.Cores <= 0 {
		c.Cores = c.Threads
	}
	if c.Vendor == "" {
		c.Vendor = defaultVendor
	}
	if c.Model == "" {
		c.Model = defaultModel
	}
	if c.Family <= 0 {
		c.Family = 6
	}
	if c.ModelNo <= 0 {
		c.ModelNo = 158
	}
	if c.MHz <= 0 {
		c.MHz = 2500
	}
	if c.MaxMHzKHz <= 0 {
		c.MaxMHzKHz = c.MHz * 1000
	}
	if len(c.Flags) == 0 {
		c.Flags = defaultCpuFlags
	}
}

// ---------------------------------------------------------------------------
// CPU topology helpers

func (c CPU) totalThreads() int { return max(c.Threads, 1) }

func (c CPU) physicalCores() int { return max(c.Cores, 1) }

func (c CPU) physicalSockets() int { return max(c.Sockets, 1) }

func (c CPU) threadsPerCore() int {
	if t := c.totalThreads() / int(c.physicalCores()); t >= 1 {
		return t
	}
	return 1
}

func (c CPU) coresPerSocket() int {
	if per := int(c.physicalCores()) / c.physicalSockets(); per >= 1 {
		return per
	}
	return int(c.physicalCores())
}

func (c CPU) maxMHzKHz() int {
	if c.MaxMHzKHz > 0 {
		return c.MaxMHzKHz
	}
	return c.MHz * 1000
}

func (c CPU) flags() []string {
	flags := append([]string{}, c.Flags...)
	if c.threadsPerCore() > 1 && !slices.Contains(flags, "ht") {
		flags = append(flags, "ht")
	}
	return flags
}

// ---------------------------------------------------------------------------
// procfs / sysfs renderers

// CpuInfo renders /proc/cpuinfo with one section per logical processor.
func (s *System) CpuInfo() string {
	c := s.CPU
	tpc := c.threadsPerCore()
	cps := c.coresPerSocket()
	mhz := intOr(c.MHz, 2500)
	var b strings.Builder
	for i := 0; i < c.totalThreads(); i++ {
		coreGlobal := i / tpc // physical core within the machine
		coreID := coreGlobal % cps
		physID := coreGlobal / cps
		fmt.Fprintf(&b, "processor\t: %d\n", i)
		fmt.Fprintf(&b, "vendor_id\t: %s\n", orDefault(c.Vendor, defaultVendor))
		fmt.Fprintf(&b, "cpu family\t: %d\n", intOr(c.Family, 6))
		fmt.Fprintf(&b, "model\t\t: %d\n", intOr(c.ModelNo, 158))
		fmt.Fprintf(&b, "model name\t: %s\n", orDefault(c.Model, defaultModel))
		fmt.Fprintf(&b, "stepping\t: 6\n")
		fmt.Fprintf(&b, "cpu MHz\t\t: %.3f\n", float64(mhz))
		fmt.Fprintf(&b, "cache size\t: %d KB\n", intOr(c.CacheKB, 16384))
		fmt.Fprintf(&b, "physical id\t: %d\n", physID)
		fmt.Fprintf(&b, "siblings\t: %d\n", tpc)
		fmt.Fprintf(&b, "core id\t\t: %d\n", coreID)
		fmt.Fprintf(&b, "cpu cores\t: %d\n", cps)
		fmt.Fprintf(&b, "apicid\t\t: %d\n", i)
		fmt.Fprintf(&b, "initial apicid\t: %d\n", i)
		fmt.Fprintf(&b, "fpu\t\t: yes\n")
		fmt.Fprintf(&b, "fpu_exception\t: yes\n")
		fmt.Fprintf(&b, "cpuid level\t: 22\n")
		fmt.Fprintf(&b, "wp\t\t: yes\n")
		fmt.Fprintf(&b, "flags\t\t: %s\n", strings.Join(c.flags(), " "))
		fmt.Fprintf(&b, "bugs\t\t: spectre_v1 spectre_v2 spec_store_bypass srbds\n")
		fmt.Fprintf(&b, "bogomips\t: %.2f\n", float64(mhz)*2)
		fmt.Fprintf(&b, "clflush size\t: 64\n")
		fmt.Fprintf(&b, "cache_alignment\t: 64\n")
		fmt.Fprintf(&b, "address sizes\t: 46 bits physical, 48 bits virtual\n")
		fmt.Fprintf(&b, "power management:\n")
	}
	return b.String()
}

// MemInfo renders /proc/meminfo in kB. Classic fields are derived from the
// modelled Total/Available so the file stays internally sane without extra
// knobs; the HugePages pool is included when configured.
func (s *System) MemInfo() string {
	totalKB := s.Memory.Total >> 10
	availKB := s.Memory.Available >> 10
	if availKB > totalKB {
		availKB = totalKB
	}
	swapTotKB := s.Memory.SwapTotal >> 10
	swapFreeKB := s.Memory.SwapFree >> 10
	if swapFreeKB > swapTotKB {
		swapFreeKB = swapTotKB
	}

	freeKB, buffersKB, cachedKB := availKB/4, totalKB/16, totalKB/8
	activeKB, inactiveKB := availKB*2/5, availKB*3/10
	var b strings.Builder
	line := func(k string, v int64) { fmt.Fprintf(&b, "%-12s %12d kB\n", k+":", v) }
	line("MemTotal", totalKB)
	line("MemFree", freeKB)
	line("MemAvailable", availKB)
	line("Buffers", buffersKB)
	line("Cached", cachedKB)
	line("SwapCached", 0)
	line("Active", activeKB)
	line("Inactive", inactiveKB)
	line("Active(anon)", availKB/5)
	line("Inactive(anon)", availKB/10)
	line("Active(file)", availKB/5)
	line("Inactive(file)", inactiveKB-buffersKB)
	line("Unevictable", 0)
	line("Mlocked", 0)
	line("SwapTotal", swapTotKB)
	line("SwapFree", swapFreeKB)
	line("Dirty", 0)
	line("Writeback", 0)
	line("AnonPages", activeKB)
	line("Mapped", cachedKB/2)
	line("Shmem", totalKB/64)
	line("Slab", cachedKB/4)
	line("SReclaimable", cachedKB/8)
	line("SUnreclaim", cachedKB/8)
	line("KernelStack", totalKB/1024)
	line("PageTables", availKB/32)
	line("CommitLimit", totalKB+swapTotKB)
	line("Committed_AS", activeKB+swapFreeKB/2)
	line("VmallocTotal", 34359738367)
	line("VmallocUsed", 0)
	line("VmallocChunk", 0)
	if s.HugePages.Total > 0 {
		line("HugePages_Total", s.HugePages.Total)
		line("HugePages_Free", s.HugePages.Free)
		line("HugePages_Rsvd", 0)
		line("HugePages_Surp", 0)
		line("Hugepagesize", s.HugePages.SizeKB)
	}
	return b.String()
}

// Swaps renders /proc/swaps: the header plus a zram entry when swap is present.
func (s *System) Swaps() string {
	var b strings.Builder
	b.WriteString("Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n")
	if s.Memory.SwapTotal > 0 {
		size := s.Memory.SwapTotal >> 10
		free := s.Memory.SwapFree >> 10
		if free > size {
			free = size
		}
		fmt.Fprintf(&b, "/dev/zram0\t\t\t\tpartition\t%d\t\t%d\t\t-2\n", size, size-free)
	}
	return b.String()
}

// Uptime renders /proc/uptime, advancing from the emulated boot time.
func (s *System) Uptime() string {
	up := time.Since(s.bootTime()).Seconds()
	return fmt.Sprintf("%.2f %.2f\n", up, up*0.75)
}

// LoadAvg renders a quiet /proc/loadavg.
func (s *System) LoadAvg() string {
	return fmt.Sprintf("0.00 0.01 0.05 %d/%d 1234\n", 1, s.CPU.totalThreads())
}

// ProcStat renders a minimal-but-compatible /proc/stat: one line per logical
// processor plus the aggregate cpu line and a few counters. The first token
// of each line is the cpu id, followed by bare jiffy values (kernel format).
func (s *System) ProcStat() string {
	per := [10]int64{987654, 12, 34567, 9000000, 9876, 0, 543, 0, 0, 0}
	var b strings.Builder
	var agg [10]int64
	line := func(id string, values []int64) {
		b.WriteString(id)
		for j := 0; j < 10; j++ {
			fmt.Fprintf(&b, " %d", values[j])
		}
		b.WriteString("\n")
	}
	for i := 0; i < s.CPU.totalThreads(); i++ {
		var v [10]int64
		for j := 0; j < 10; j++ {
			v[j] = per[j] + int64(i)*1000*(int64(j)%3)
			agg[j] += v[j]
		}
		line(fmt.Sprintf("cpu%d", i), v[:])
	}
	line("cpu", agg[:])
	fmt.Fprintf(&b, "intr %d\n", 0)
	fmt.Fprintf(&b, "ctxt %d\n", 1234567)
	fmt.Fprintf(&b, "btime %d\n", s.bootTime().Unix())
	fmt.Fprintf(&b, "processes %d\n", 1)
	fmt.Fprintf(&b, "procs_running %d\n", 1)
	fmt.Fprintf(&b, "procs_blocked %d\n", 0)
	return b.String()
}

// ProcVersion renders /proc/version.
func (s *System) ProcVersion() string {
	return fmt.Sprintf("Linux version %s (%s) (%s) %s\n",
		s.Kernel.Release, orDefault(s.Kernel.Builder, "build@sandbox"),
		orDefault(s.Kernel.Toolchain, "gcc (GCC) 12.2.0"), s.Kernel.Version)
}

// KernelRelease renders /proc/sys/kernel/osrelease.
func (s *System) KernelRelease() string { return s.Kernel.Release + "\n" }

// KernelVersion renders /proc/sys/kernel/version.
func (s *System) KernelVersion() string { return s.Kernel.Version + "\n" }

// BootID renders /proc/sys/kernel/boot_id and random/boot_id. The value is
// generated once per resolved model and then stable.
func (s *System) BootID() string {
	if s.Kernel.BootID == "" {
		s.Kernel.BootID = randomUUID()
	}
	return s.Kernel.BootID + "\n"
}

// DomainName renders /proc/sys/kernel/domainname.
func (s *System) DomainName() string { return orDefault(s.Kernel.Domain, defaultDomain) + "\n" }

// CpuList renders the online/possible/present cpu list for sysfs, e.g. "0-3".
func (s *System) CpuList() string {
	if n := s.CPU.totalThreads(); n > 1 {
		return fmt.Sprintf("0-%d\n", n-1)
	}
	return "0\n"
}

// CpuSysfs serves per-cpu sysfs files (/sys/devices/system/cpu/cpuN/...).
// It returns "" for paths it does not model so callers can pass through.
func (s *System) CpuSysfs(path string) (string, error) {
	for i := 0; i < s.CPU.totalThreads(); i++ {
		prefix := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/", i)
		rest, ok := strings.CutPrefix(path, prefix)
		if !ok {
			continue
		}
		if content, found := s.cpuSysfsFile(i, rest); found {
			return content, nil
		}
	}
	return "", nil
}

// cpuSysfsFile renders one modeled per-cpu sysfs file for cpu i. ok is false
// when rel is not a modeled file.
func (s *System) cpuSysfsFile(i int, rel string) (string, bool) {
	c := s.CPU
	switch rel {
	case "online":
		return "1\n", true
	case "cpufreq/cpuinfo_max_freq":
		return fmt.Sprintf("%d\n", c.maxMHzKHz()), true
	case "cpufreq/cpuinfo_min_freq":
		return fmt.Sprintf("%d\n", c.maxMHzKHz()*8/10), true
	case "topology/core_id":
		return fmt.Sprintf("%d\n", (i/c.threadsPerCore())%c.coresPerSocket()), true
	case "topology/physical_package_id":
		return fmt.Sprintf("%d\n", (i/c.threadsPerCore())/c.coresPerSocket()), true
	case "topology/thread_siblings_list":
		if tpc := c.threadsPerCore(); tpc > 1 {
			core0 := (i / tpc) * tpc
			return fmt.Sprintf("%d-%d\n", core0, core0+tpc-1), true
		}
		return fmt.Sprintf("%d\n", i), true
	case "topology/core_siblings_list":
		return s.CpuList(), true
	}
	return "", false
}

// SysfsFile is a single regular file in the materialized /sys/devices/system
// tree, rendered from the model.
type SysfsFile struct {
	Path    string
	Content string
}

// SysfsDevices renders the emulated /sys/devices/system tree as regular
// files, so the tree needs no seccomp emulation. Setup writes these files
// into the rootfs twice from the same renderer: the child's boot
// materialization and the parent's injected copy through the transferred
// rootfs fd (see Sandbox.bootFiles). Content is derived from the model so it
// stays coherent with /proc/*; it can be rewritten at runtime via
// Sandbox.InjectFiles.
func (s *System) SysfsDevices() []SysfsFile {
	c := s.CPU
	var files []SysfsFile

	cpu := "/sys/devices/system/cpu"
	files = append(files,
		SysfsFile{Path: cpu + "/online", Content: s.CpuList()},
		SysfsFile{Path: cpu + "/possible", Content: s.CpuList()},
		SysfsFile{Path: cpu + "/present", Content: s.CpuList()},
		SysfsFile{Path: cpu + "/offline", Content: ""},
	)
	for i := 0; i < c.totalThreads(); i++ {
		base := fmt.Sprintf("%s/cpu%d", cpu, i)
		for _, rel := range []string{
			"online",
			"cpufreq/cpuinfo_max_freq",
			"cpufreq/cpuinfo_min_freq",
			"topology/core_id",
			"topology/physical_package_id",
			"topology/thread_siblings_list",
			"topology/core_siblings_list",
		} {
			if content, ok := s.cpuSysfsFile(i, rel); ok {
				files = append(files, SysfsFile{Path: base + "/" + rel, Content: content})
			}
		}
	}

	node := "/sys/devices/system/node/node0"
	files = append(files,
		SysfsFile{Path: node + "/cpulist", Content: s.CpuList()},
		SysfsFile{Path: node + "/possible", Content: "0\n"},
		SysfsFile{Path: node + "/meminfo", Content: s.NodeMeminfo()},
		SysfsFile{Path: node + "/distance", Content: "10\n"},
	)
	for i := 0; i < c.totalThreads(); i++ {
		for _, rel := range []string{"topology/core_id", "topology/physical_package_id"} {
			if content, ok := s.cpuSysfsFile(i, rel); ok {
				files = append(files, SysfsFile{Path: fmt.Sprintf("%s/cpu%d/%s", node, i, rel), Content: content})
			}
		}
	}

	return files
}

// NodeMeminfo renders /sys/devices/system/node/node0/meminfo in kB, using the
// same family of values as /proc/meminfo so node memory and total memory stay
// coherent.
func (s *System) NodeMeminfo() string {
	totalKB := s.Memory.Total >> 10
	availKB := s.Memory.Available >> 10
	if availKB > totalKB {
		availKB = totalKB
	}
	freeKB := availKB / 4
	var b strings.Builder
	line := func(k string, v int64) { fmt.Fprintf(&b, "Node 0 %-12s %12d kB\n", k+":", v) }
	line("MemTotal", totalKB)
	line("MemFree", freeKB)
	line("MemAvailable", availKB)
	return b.String()
}

// Utsname builds the utsname struct for uname(2) spoofing.
func (s *System) Utsname(hostname string) *unix.Utsname {
	uts := &unix.Utsname{}
	put := func(dst []byte, v string) { copy(dst, v) }
	put(uts.Sysname[:], "Linux")
	put(uts.Nodename[:], hostname)
	put(uts.Release[:], s.Kernel.Release)
	put(uts.Version[:], s.Kernel.Version)
	put(uts.Machine[:], orDefault(s.Arch, "x86_64"))
	put(uts.Domainname[:], orDefault(s.Kernel.Domain, defaultDomain))
	return uts
}

// ---------------------------------------------------------------------------
// helpers

func (s *System) bootTime() time.Time {
	if s.booted.IsZero() {
		s.booted = time.Now()
	}
	return s.booted
}

func intOr(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func archForGOARCH(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "386":
		return "i686"
	case "arm":
		return "armv7l"
	default:
		return goarch
	}
}

func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000" // never expected
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
