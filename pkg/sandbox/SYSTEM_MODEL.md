# System Model

The sandbox exposes a typed, emulated view of Linux hardware and resources to
processes running inside it. This document describes the model, the files it
renders, and how it is wired into the seccomp supervisor.

## Overview

`pkg/sandbox/system.go` defines a `System` struct — a small, explicit model of
the machine a sandbox should *look like* — plus renderers that produce
compatible contents for the procfs/sysfs files programs commonly read
(`/proc/cpuinfo`, `/proc/meminfo`, `uname(2)`, …).

The model is deliberately minimal: a handful of typed knobs (CPU topology,
memory, huge pages, kernel identity), with every rendered file derived from
those knobs so the data stays internally consistent. It does not attempt to
model a real kernel.

The model is JSON-serializable and carried in the sandbox `Config`, so callers
can size the emulated node. When no model is configured, `DefaultSystem()` is
used.

## The model

```go
type System struct {
    Arch      string    // uname machine, e.g. "x86_64"
    CPU       CPU
    Memory    Memory
    HugePages HugePages
    Kernel    Kernel
}

type CPU struct {
    Vendor    string   // e.g. "GenuineIntel"
    Model     string   // model name line in /proc/cpuinfo
    Family    int      // "cpu family"
    ModelNo   int      // "model"
    Sockets   int      // physical packages
    Cores     int      // physical cores
    Threads   int      // logical processors (SMT threads)
    MHz       int      // nominal clock
    MaxMHzKHz int      // cpufreq cpuinfo_max_freq, kHz
    CacheKB   int      // L3 cache size
    Flags     []string // cpuid feature flags
}

type Memory struct {
    Total     int64 // bytes
    Available int64 // bytes
    SwapTotal int64 // bytes
    SwapFree  int64 // bytes
}

type HugePages struct {
    Total  int64 // number of huge pages
    Free   int64 // number currently free
    SizeKB int64 // page size in kB, e.g. 2048
}

type Kernel struct {
    Release   string // uname -r, e.g. "6.6.26-sandbox"
    Version   string // uname -v and sys/kernel/version, e.g. "#1 SMP Fri Jan  1 ..."
    Builder   string // /proc/version build host
    Toolchain string // /proc/version compiler string
    BootID    string // /proc/sys/kernel/{,random/}boot_id
    Domain    string // uname domainname
}
```

### CPU topology

`Threads` is the total logical processors, `Cores` the total physical cores,
`Sockets` optional (defaults to one package). The renderer derives a consistent
SMT mapping:

- **threads per core** = `Threads / Cores` (min 1)
- **cores per socket** = `Cores / Sockets`

This drives both `/proc/cpuinfo` (matching `core id`, `physical id`,
`siblings`, `cpu cores`, plus the `ht` flag when SMT > 1) and the sysfs
topology files.

## Rendered files

All renderers are `System` methods returning the exact file content (with a
trailing newline).

| Path | Method | Notes |
|------|--------|-------|
| `/proc/cpuinfo` | `CpuInfo()` | one section per logical processor |
| `/proc/meminfo` | `MemInfo()` | kB values; classic fields derived from Total/Available; HugePages* when configured |
| `/proc/swaps` | `Swaps()` | header + zram entry only when swap > 0 |
| `/proc/stat` | `ProcStat()` | bare jiffy values per kernel format; per-cpu + aggregate `cpu` line; `btime` from boot time |
| `/proc/uptime` | `Uptime()` | advances from the recorded boot time |
| `/proc/loadavg` | `LoadAvg()` | quiet load average |
| `/proc/version` | `ProcVersion()` | `Linux version <release> (<builder>) (<toolchain>) <version>` |
| `/proc/sys/kernel/osrelease` | `KernelRelease()` | `uname -r` |
| `/proc/sys/kernel/version` | `KernelVersion()` | `uname -v` |
| `/proc/sys/kernel/boot_id` | `BootID()` | random UUID v4, stable per sandbox |
| `/proc/sys/kernel/random/boot_id` | `BootID()` | same value |
| `/proc/sys/kernel/domainname` | `DomainName()` | `uname` domain |
| `/sys/devices/system/cpu/{online,possible,present}` | `CpuList()` | e.g. `0-3` |
| `/sys/devices/system/cpu/offline` | — | empty file |
| `/sys/devices/system/cpu/cpuN/...` | `cpuSysfsFile(i, rel)` | per-cpu cpufreq + topology |
| `/sys/devices/system/node/node0/{cpulist,possible}` | `CpuList()` / `0` | single node |
| `/sys/devices/system/node/node0/meminfo` | `NodeMeminfo()` | kB; coherent with `/proc/meminfo` |
| `/sys/devices/system/node/node0/distance` | — | `10` (local NUMA distance) |
| `uname(2)` | `Utsname(hostname)` | full `utsname` struct |

### Materialized /sys tree

The `/sys/devices/system` files are rendered as **regular files** in the
sandbox rootfs during setup, not served from seccomp. `SysfsDevices()` returns
the full set as `SysfsFile{Path, Content}` pairs; `pkg/sandbox/sandbox.go`
`materializeSysfs()` writes them with `os.MkdirAll`/`os.WriteFile` before the
config payloads are applied (so an explicit payload can still override a file).

Per-cpu content is shared by `cpuSysfsFile(i, rel)` and the thin public
wrapper `CpuSysfs(path)`:

- `online` → `1`
- `cpufreq/cpuinfo_max_freq` → `MaxMHzKHz`
- `cpufreq/cpuinfo_min_freq` → 80% of max
- `topology/core_id`, `topology/physical_package_id`
- `topology/thread_siblings_list`, `topology/core_siblings_list`

The node0 subtree renders `cpulist`/`possible` from the CPU topology, `meminfo`
from the `Memory` model, and `distance` as `10` for the single emulated node.
Because these are real files, `stat(2)`/`statx(2)` succeed and reads resolve
natively.

## Network identity

The sandbox's network namespace carries one emulated default interface
(`eth0`, a dummy device) in addition to loopback. Its L2/L3 identity — MAC
address, private IPv4 and ULA IPv6 — is derived deterministically from the
machine id by `pkg/netid.FromMachineID` (docs/TODO.md, "Dynamic network
interface"): predictable across restarts, never hardcoded.

- A machine id that is exactly 16 bytes (32 hex characters) is used as-is;
  any other value is SHA-256 hashed to 16 bytes
- **MAC** — the first 6 identity bytes with the unicast bit cleared and the
  locally-administered bit set, so it can never collide with a real vendor
  OUI
- **IPv4** — `10.<b12>.<b13>.<b14>`, a 10/8 private address
- **IPv6** — `fd00:<7 groups from the identity>`, a ULA `fd00::/8` address
- **loopback** — `lo` keeps its kernel-assigned `127.0.0.1/8` and `::1/128`

The interface is created and brought up in `Sandbox.setupNetwork()` during
boot; `Config.IPAddress`, when set, overrides the derived IPv4. Node/kubelet
addresses, clusterDNS and the CRI pod sandbox IP derive from the same
function so the emulated identity is coherent across the stack.

## Resolution

`System.Resolve()` fills optional fields with defaults, generates a stable boot
id (when not already set), and records the emulated boot time so `/proc/uptime`
and `/proc/stat`'s `btime` advance monotonically. It is safe on a
shared/JSON-decoded model.

`pkg/sandbox/sandbox.go`:

```go
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
```

The model is resolved once per sandbox and memoized on the `Sandbox`, so
identity (boot id, boot time) stays stable for the lifetime of the sandbox.

## Wiring into seccomp

`pkg/sandbox/filter_files.go` builds the `seccomp.SupervisorConfig`. Exact
paths under `/proc` and `/proc/sys/kernel` are spoofed with
`seccomp.SpoofFile(sys.<renderer>())`, and `uname(2)` is spoofed with a
precomputed `seccomp.SpoofUname(sys.Utsname(...))`. The `/sys/devices/system`
tree is *not* intercepted: it is materialized as real files (see above). A
catch-all `PassThrough` (registered last) lets everything else resolve
natively. Route order matters: exact matches are registered before the catch-all
so they take precedence.

The mount table (`/proc/mounts`) remains a static value in `filter_files.go`;
swap and the previously hardcoded CPU frequency are now generated from the model.

## Defaults

`DefaultSystem()` provides a small deterministic emulation: 16 cores / 8 GiB RAM
with 2 GiB swap, a 2 MiB hugetlb pool, and a `6.6.26-sandbox` kernel. Callers
provide their own `Config.System` to size a node differently.

## Tests

`pkg/sandbox/system_test.go` covers:

- `/proc/cpuinfo` section count and SMT topology mapping
- `/proc/meminfo` values and huge-page lines
- `/proc/swaps` with and without swap
- `/proc/version` kernel format, `osrelease`
- boot-id stability/uniqueness and UUID v4 shape
- `uname` field population and NUL-termination
- per-cpu sysfs rendering and pass-through behavior
- `/sys/devices/system` tree rendering (`SysfsDevices`) and node memory (`NodeMeminfo`)
- `/proc/stat` cpu line count and `btime`
- `Config.System` JSON round-trip through the sandbox cookie