package sandbox

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func testSystem() *System {
	m := System{
		Arch: "x86_64",
		CPU:  CPU{Vendor: "GenuineIntel", Model: "test cpu", Family: 6, Sockets: 1, Cores: 4, Threads: 8, MHz: 2400, MaxMHzKHz: 2400000, CacheKB: 16384},
		Memory: Memory{
			Total:     16 << 30,
			Available: 12 << 30,
			SwapTotal: 2 << 30,
			SwapFree:  1 << 30,
		},
		HugePages: HugePages{Total: 128, Free: 64, SizeKB: 2048},
		Kernel: Kernel{
			Release: "6.6.26-sandbox", Version: "#1 SMP Fri Jan  1 00:00:00 UTC 2024",
			Builder: "build@sandbox", Toolchain: "gcc (GCC) 12.2.0", Domain: "localdomain",
		},
	}
	return m.Resolve()
}

// cstr converts a NUL-terminated byte array to a Go string.
func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// parseMeminfoLine parses a /proc/meminfo line into key and kB value.
func parseMeminfoLine(line string) (key string, kb int64, ok bool) {
	key, rest, found := strings.Cut(line, ":")
	if !found {
		return "", 0, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return strings.TrimSpace(key), v, true
}

func TestCpuInfoSections(t *testing.T) {
	sys := testSystem()
	info := sys.CpuInfo()
	if n := strings.Count(info, "processor\t:"); n != sys.CPU.Threads {
		t.Errorf("expected %d processor sections, got %d", sys.CPU.Threads, n)
	}
	if !strings.Contains(info, "vendor_id\t: GenuineIntel") {
		t.Errorf("cpuinfo missing vendor_id:\n%s", info)
	}
	if !strings.Contains(info, "flags\t\t: ") {
		t.Errorf("cpuinfo missing flags line:\n%s", info)
	}
}

func TestCpuInfoSMTTopology(t *testing.T) {
	sys := testSystem() // 8 threads / 4 cores / 1 socket
	info := sys.CpuInfo()
	mapping := map[int]string{}
	cpu := -1
	for _, l := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(l, "processor\t: "); ok {
			cpu, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		if v, ok := strings.CutPrefix(l, "core id\t\t: "); ok && cpu >= 0 {
			mapping[cpu] = strings.TrimSpace(v)
		}
	}
	// threads 0/1 share core 0, threads 2/3 share core 1, ... core 7 -> 3.
	want := map[int]string{0: "0", 1: "0", 2: "1", 3: "1", 6: "3", 7: "3"}
	for cpu, core := range want {
		if mapping[cpu] != core {
			t.Errorf("cpu%d core id = %q, want %q (mapping=%v)", cpu, mapping[cpu], core, mapping)
		}
	}
	if !strings.Contains(info, "siblings\t: 2\n") {
		t.Errorf("expected siblings=2 for SMT topology")
	}
	if !strings.Contains(info, "cpu cores\t: 4\n") {
		t.Errorf("expected cpu cores=4")
	}
	if !strings.Contains(info, " ht\n") {
		t.Errorf("expected ht flag with SMT")
	}
}

func TestMemInfo(t *testing.T) {
	sys := testSystem()
	info := sys.MemInfo()
	vals := map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(info))
	for sc.Scan() {
		k, v, ok := parseMeminfoLine(sc.Text())
		if !ok {
			t.Fatalf("unparseable line %q", sc.Text())
		}
		vals[k] = v
	}
	if vals["MemTotal"] != 16<<30>>10 {
		t.Errorf("MemTotal = %d, want %d", vals["MemTotal"], 16<<30>>10)
	}
	if vals["MemAvailable"] > vals["MemTotal"] {
		t.Errorf("MemAvailable %d > MemTotal %d", vals["MemAvailable"], vals["MemTotal"])
	}
	if vals["SwapTotal"] != 2<<30>>10 {
		t.Errorf("SwapTotal = %d, want %d", vals["SwapTotal"], 2<<30>>10)
	}
	if vals["HugePages_Total"] != 128 || vals["Hugepagesize"] != 2048 {
		t.Errorf("hugepages: Total=%d size=%d", vals["HugePages_Total"], vals["Hugepagesize"])
	}
}

func TestSwaps(t *testing.T) {
	sys := testSystem()
	if s := sys.Swaps(); !strings.Contains(s, "/dev/zram0") || !strings.Contains(s, "Priority") {
		t.Errorf("swaps missing zram entry:\n%q", s)
	}
	noswap := (&System{Memory: Memory{Total: 1 << 30, Available: 1 << 30}}).Resolve()
	if s := noswap.Swaps(); strings.Contains(s, "zram0") {
		t.Errorf("swaps should be header-only without swap:\n%q", s)
	}
}

func TestProcVersion(t *testing.T) {
	sys := testSystem()
	v := sys.ProcVersion()
	re := regexp.MustCompile(`^Linux version \S+ \(\S+\) \(.+\) #\d+ SMP \w+ \w+ +\d+ \d+:\d+:\d+ \w+ \d+\n$`)
	if !re.MatchString(v) {
		t.Errorf("proc version %q does not match kernel format", v)
	}
	if got := sys.KernelRelease(); got != sys.Kernel.Release+"\n" {
		t.Errorf("osrelease = %q", got)
	}
}

func TestBootIDStableAndUnique(t *testing.T) {
	a, b := testSystem(), testSystem()
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\n$`)
	if !uuid.MatchString(a.BootID()) {
		t.Errorf("boot id not a v4 uuid: %q", a.BootID())
	}
	if a.BootID() != a.BootID() {
		t.Errorf("boot id not stable within a sandbox")
	}
	if a.BootID() == b.BootID() {
		t.Errorf("boot id must differ across sandboxes")
	}
}

func TestUtsname(t *testing.T) {
	sys := testSystem()
	uts := sys.Utsname("node-1")
	if got := cstr(uts.Sysname[:]); got != "Linux" {
		t.Errorf("sysname = %q", got)
	}
	if got := cstr(uts.Nodename[:]); got != "node-1" {
		t.Errorf("nodename = %q", got)
	}
	if got := cstr(uts.Release[:]); got != sys.Kernel.Release {
		t.Errorf("release = %q", got)
	}
	if got := cstr(uts.Version[:]); got != sys.Kernel.Version {
		t.Errorf("version = %q", got)
	}
	if got := cstr(uts.Machine[:]); got != "x86_64" {
		t.Errorf("machine = %q", got)
	}
	if got := cstr(uts.Domainname[:]); got != "localdomain" {
		t.Errorf("domainname = %q", got)
	}
}

func TestCpuSysfs(t *testing.T) {
	sys := testSystem() // 8 threads: online list 0-7, threads 0/1 -> core 0
	if got := sys.CpuList(); got != "0-7\n" {
		t.Errorf("cpu list = %q", got)
	}
	cases := map[string]string{
		"/sys/devices/system/cpu/cpu0/online":                        "1\n",
		"/sys/devices/system/cpu/cpu1/topology/core_id":              "0\n",
		"/sys/devices/system/cpu/cpu2/topology/core_id":              "1\n",
		"/sys/devices/system/cpu/cpu7/topology/core_id":              "3\n",
		"/sys/devices/system/cpu/cpu0/topology/physical_package_id":  "0\n",
		"/sys/devices/system/cpu/cpu0/topology/thread_siblings_list": "0-1\n",
		"/sys/devices/system/cpu/cpu4/topology/core_siblings_list":   "0-7\n",
		"/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq":      "2400000\n",
		"/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_min_freq":      "1920000\n",
		"/sys/devices/system/cpu/cpu1/topology/thread_siblings_list": "0-1\n",
	}
	for path, want := range cases {
		got, err := sys.CpuSysfs(path)
		if err != nil || got != want {
			t.Errorf("CpuSysfs(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	if got, _ := sys.CpuSysfs("/sys/devices/system/cpu/cpu0/topology/nope"); got != "" {
		t.Errorf("unknown path should pass through, got %q", got)
	}
	if got, _ := sys.CpuSysfs("/sys/devices/system/node/node0/meminfo"); got != "" {
		t.Errorf("node path should pass through, got %q", got)
	}
}

func TestSysfsDevices(t *testing.T) {
	sys := testSystem() // 8 threads, 16 GiB total / 12 GiB available

	files := map[string]string{}
	for _, f := range sys.SysfsDevices() {
		if _, dup := files[f.Path]; dup {
			t.Errorf("duplicate path %q", f.Path)
		}
		files[f.Path] = f.Content
	}

	// Root cpu lists.
	cpu := "/sys/devices/system/cpu"
	for _, rel := range []string{"online", "possible", "present"} {
		if got := files[cpu+"/"+rel]; got != sys.CpuList() {
			t.Errorf("%s = %q, want %q", rel, got, sys.CpuList())
		}
	}
	if got := files[cpu+"/offline"]; got != "" {
		t.Errorf("offline = %q, want empty", got)
	}

	// Per-cpu files agree with CpuSysfs and exist for every cpu.
	for i := 0; i < sys.CPU.Threads; i++ {
		for _, rel := range []string{
			"online", "cpufreq/cpuinfo_max_freq", "cpufreq/cpuinfo_min_freq",
			"topology/core_id", "topology/physical_package_id",
			"topology/thread_siblings_list", "topology/core_siblings_list",
		} {
			p := fmt.Sprintf("%s/cpu%d/%s", cpu, i, rel)
			want, _ := sys.CpuSysfs(p)
			if got := files[p]; got != want {
				t.Errorf("%s = %q, want %q", p, got, want)
			}
		}
	}

	// Node0 subtree.
	node := "/sys/devices/system/node/node0"
	if got := files[node+"/cpulist"]; got != sys.CpuList() {
		t.Errorf("node0 cpulist = %q, want %q", got, sys.CpuList())
	}
	if got := files[node+"/possible"]; got != "0\n" {
		t.Errorf("node0 possible = %q, want %q", got, "0\n")
	}
	if got := files[node+"/distance"]; got != "10\n" {
		t.Errorf("node0 distance = %q, want %q", got, "10\n")
	}
	if mi := files[node+"/meminfo"]; !strings.Contains(mi, "Node 0 MemTotal:") {
		t.Errorf("node0 meminfo missing MemTotal:\n%s", mi)
	}

	// Count: 4 cpu-list files + 7*T per-cpu + 4 node0 + 2*T node0 cpu.
	wantCount := 4 + 7*sys.CPU.Threads + 4 + 2*sys.CPU.Threads
	if len(files) != wantCount {
		t.Errorf("rendered %d files, want %d", len(files), wantCount)
	}
}

func TestNodeMeminfo(t *testing.T) {
	sys := testSystem() // 16 GiB total / 12 GiB available
	mi := sys.NodeMeminfo()
	vals := map[string]int64{}
	sc := bufio.NewScanner(strings.NewReader(mi))
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "Node 0 ")
		if !ok {
			t.Fatalf("unexpected line %q", sc.Text())
		}
		k, v, ok := parseMeminfoLine(rest)
		if !ok {
			t.Fatalf("unparseable line %q", sc.Text())
		}
		vals[k] = v
	}
	if vals["MemTotal"] != 16<<30>>10 {
		t.Errorf("MemTotal = %d, want %d", vals["MemTotal"], 16<<30>>10)
	}
	if vals["MemAvailable"] > vals["MemTotal"] {
		t.Errorf("MemAvailable %d > MemTotal %d", vals["MemAvailable"], vals["MemTotal"])
	}
}

func TestProcStatBtime(t *testing.T) {
	sys := testSystem()
	stat := sys.ProcStat()
	if strings.Count(stat, "cpu ") != 1 {
		t.Errorf("expected one aggregate cpu line:\n%s", stat)
	}
	lines := bufio.NewScanner(strings.NewReader(stat))
	cpuLines := 0
	for lines.Scan() {
		if strings.HasPrefix(lines.Text(), "cpu") {
			cpuLines++
		}
	}
	if cpuLines != sys.CPU.Threads+1 {
		t.Errorf("expected %d cpu lines, got %d", sys.CPU.Threads+1, cpuLines)
	}
}

func TestDefaultSystem(t *testing.T) {
	sys := DefaultSystem().Resolve()
	if sys.CPU.Threads <= 0 || sys.Memory.Total <= 0 {
		t.Errorf("default system not sane: %+v", sys.CPU)
	}
	if sys.Kernel.BootID == "" {
		t.Errorf("missing boot id")
	}
}

func TestConfigSystemRoundTrip(t *testing.T) {
	c := &Config{
		Version: "v1", Hostname: "h", MachineID: "m", IPAddress: "10.0.0.1",
		System: testSystem(),
	}
	ck, err := writeCookie(c)
	if err != nil {
		t.Fatal(err)
	}
	back, err := readCookie(ck)
	if err != nil {
		t.Fatal(err)
	}
	if back.System == nil {
		t.Fatal("System lost in cookie round-trip")
	}
	if back.System.CPU.Threads != c.System.CPU.Threads {
		t.Errorf("threads = %d, want %d", back.System.CPU.Threads, c.System.CPU.Threads)
	}
}
