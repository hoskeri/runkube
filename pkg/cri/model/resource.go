package model

import (
	"fmt"
	"math"
	"time"

	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Default load profile (docs/cri-model-redesign.md, decision D2).
const (
	// DefaultLoadIdle is the utilization fraction of a non-running
	// container (CREATED/EXITED).
	DefaultLoadIdle = 0.02
	// DefaultLoadActive is the utilization fraction of a running container.
	DefaultLoadActive = 0.35
)

// DefaultTickDuration is the simulated wall-clock duration of one tick,
// used to derive timestamps and byte rates (decision D3).
const DefaultTickDuration = 100 * time.Millisecond

// OpKind identifies a simulated operation whose duration Hooks.Duration may
// override (falling back to Options.Delays) (decision D5).
type OpKind int

// Sandbox operation constants.
const (
	OpSandboxStart OpKind = iota
	OpSandboxStop
	OpContainerStart
	OpContainerStop
	OpImagePull
)

// DelayConfig is the default simulated duration of each operation kind, in
// ticks. The defaults mirror the historical hardcoded 10 ticks in cri.go
// (≈1s at a 100ms tick) (decision D5).
type DelayConfig struct {
	SandboxStart   Ticks
	SandboxStop    Ticks
	ContainerStart Ticks
	ContainerStop  Ticks
	ImagePull      Ticks
}

func defaultDelays() DelayConfig {
	return DelayConfig{
		SandboxStart:   10,
		SandboxStop:    10,
		ContainerStart: 10,
		ContainerStop:  10,
		ImagePull:      10,
	}
}

func (d DelayConfig) forKind(k OpKind) Ticks {
	switch k {
	case OpSandboxStart:
		return d.SandboxStart
	case OpSandboxStop:
		return d.SandboxStop
	case OpContainerStart:
		return d.ContainerStart
	case OpContainerStop:
		return d.ContainerStop
	case OpImagePull:
		return d.ImagePull
	}
	return 0
}

// ObjectKind identifies what kind of object an ObjectRef points at.
type ObjectKind int

// ObjectSandbox represents a Kubernetes Object reference for a sandbox
const (
	ObjectSandbox ObjectKind = iota
	ObjectContainer
	ObjectImage
)

// ObjectRef identifies an object to a hook.
type ObjectRef struct {
	Kind ObjectKind
	ID   string
}

// Hooks are user-supplied override functions. Every hook has a default;
// unset hooks fall back to it. Hooks run on the simulation driver (or the
// calling goroutine for Do), outside the runtime lock: use the public API
// for reads and runtime.Do for atomic writes. See docs/cri-model-usage.md.
type Hooks struct {
	// Load returns the utilization fraction of ref at simulated time now.
	// Values below 0 are clamped to 0. With LimitEnforcement enabled, a
	// value >= 1 makes a running container eligible for OOM. The default is
	// state-based: LoadActive while running, LoadIdle otherwise.
	Load func(rt *Runtime, ref ObjectRef, now Ticks) float64

	// Duration returns the simulated duration of an operation, in ticks.
	// The default is the Options.Delays table.
	Duration func(kind OpKind) Ticks
}

// Options configures a Runtime. Zero-valued options take their documented
// defaults.
type Options struct {
	// TickRate is the wall-clock pacing of the adapter goroutine. 0 selects
	// a manually driven clock (Advance/Step), used by tests (decision D3).
	TickRate time.Duration

	// TickDuration is the simulated wall-clock duration of one tick, used
	// to derive timestamps and byte rates. Default 100ms.
	TickDuration time.Duration

	// Delays is the default simulated duration of each operation kind.
	// Defaults: 10 ticks each (decision D5).
	Delays DelayConfig

	// Hooks override model behavior.
	Hooks Hooks

	// LoadIdle / LoadActive are the default utilization fractions for
	// non-running / running containers. Defaults: 0.02 / 0.35 (D2).
	LoadIdle   float64
	LoadActive float64

	// LimitEnforcement enables OOM simulation: a running container whose
	// load reaches 1.0 is transitioned to EXITED with reason OOMKilled.
	// Default false (decision D4).
	LimitEnforcement bool
}

// ResourceSpec is the static resource profile of a container (limits).
type ResourceSpec struct {
	CPUMillis int64
	MemBytes  int64
	NetRxBps  int64
	NetTxBps  int64
}

// DefaultResourceSpec is the profile used when none is assigned.
func DefaultResourceSpec() ResourceSpec {
	return ResourceSpec{
		CPUMillis: 100,
		MemBytes:  128 << 20,
		NetRxBps:  1 << 20,
		NetTxBps:  1 << 20,
	}
}

// Usage is the accumulated resource accounting for an object. Cumulative
// counters (CPUCoreNanos, PageFaults, Net*Bytes) grow monotonically; the
// remaining fields are rates or instantaneous samples at the last tick.
type Usage struct {
	CPUCoreNanos  uint64
	CPUNanoCores  uint64
	MemUsageBytes uint64
	MemWorkingSet uint64
	MemRSS        uint64
	PageFaults    uint64
	NetRxBytes    uint64
	NetTxBytes    uint64
}

// New creates a Runtime with the given options.
func New(opts Options) *Runtime {
	if opts.TickDuration <= 0 {
		opts.TickDuration = DefaultTickDuration
	}
	if opts.Delays == (DelayConfig{}) {
		opts.Delays = defaultDelays()
	}
	if opts.LoadIdle == 0 {
		opts.LoadIdle = DefaultLoadIdle
	}
	if opts.LoadActive == 0 {
		opts.LoadActive = DefaultLoadActive
	}
	r := &Runtime{
		sandboxes:  make(map[string]*Sandbox),
		containers: make(map[string]*Container),
		images:     make(map[string]*Image),
		subs:       make(map[uint64]*Subscription),
		opts:       opts,
		startTime:  time.Now(),
	}
	if opts.TickRate > 0 {
		r.startWallClock()
	}
	return r
}

// Duration returns the simulated duration of an operation kind, consulting
// Hooks.Duration first, then Options.Delays.
func (r *Runtime) Duration(kind OpKind) Ticks {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.opts.Hooks.Duration != nil {
		return r.opts.Hooks.Duration(kind)
	}
	return r.opts.Delays.forKind(kind)
}

// SetContainerSpec returns a Mutation that assigns a resource profile to a
// container. It fails if the container does not exist.
func SetContainerSpec(id string, spec ResourceSpec) Mutation {
	return func(rt *Runtime) error {
		c, ok := rt.containers[id]
		if !ok {
			return fmt.Errorf("model: container %q not found", id)
		}
		c.spec = spec
		return nil
	}
}

// simTime converts the current simulation clock to wall time. Call with the
// runtime lock held.
func (r *Runtime) simTime() time.Time {
	return r.startTime.Add(time.Duration(r.now) * r.opts.TickDuration)
}

// loadFor returns the utilization of a container at time now. Hooks.Load
// takes precedence; the default is state-based. Must not be called while
// holding the runtime lock (hooks may use the public API).
func (r *Runtime) loadFor(id string, state criv1.ContainerState, now Ticks) float64 {
	if r.opts.Hooks.Load != nil {
		return r.opts.Hooks.Load(r, ObjectRef{Kind: ObjectContainer, ID: id}, now)
	}
	if state == criv1.ContainerState_CONTAINER_RUNNING {
		return r.opts.LoadActive
	}
	return r.opts.LoadIdle
}

// account advances resource usage accounting by delta ticks. It snapshots
// object state under the lock, invokes load hooks outside the lock, then
// applies the resulting deltas under the lock.
func (r *Runtime) account(delta Ticks) {
	if delta == 0 {
		return
	}
	type cSnap struct {
		id    string
		state criv1.ContainerState
		spec  ResourceSpec
		ws    uint64
	}
	r.mu.Lock()
	now := r.now
	snaps := make([]cSnap, 0, len(r.containers))
	for id, c := range r.containers {
		snaps = append(snaps, cSnap{id: id, state: c.state(), spec: c.spec, ws: c.usage.MemWorkingSet})
	}
	r.mu.Unlock()

	type cDelta struct {
		id         string
		cpuNanos   uint64
		cpuCores   uint64
		ws         uint64
		usageBytes uint64
		rss        uint64
		faults     uint64
		rxBytes    uint64
		txBytes    uint64
		oom        bool
	}
	deltas := make([]cDelta, 0, len(snaps))
	secPerTick := r.opts.TickDuration.Seconds()
	for _, s := range snaps {
		u := r.loadFor(s.id, s.state, now)
		if u < 0 {
			u = 0
		}
		spec := s.spec
		if spec == (ResourceSpec{}) {
			spec = DefaultResourceSpec()
		}
		// CPU: cumulative usage-core-nanos accumulates the integrated load;
		// nano-cores is the instantaneous rate (core-nanos per second).
		cpuNanos := uint64(u * float64(spec.CPUMillis) * 1e6 * secPerTick * float64(delta))
		cpuCores := uint64(u * float64(spec.CPUMillis) * 1e6)
		// Memory: working set ramps exponentially toward limit*u, capped at
		// the limit; page faults accrue with growth.
		target := float64(spec.MemBytes) * u
		if target > float64(spec.MemBytes) {
			target = float64(spec.MemBytes)
		}
		decay := math.Pow(0.9, float64(delta))
		newWS := target - (target-float64(s.ws))*decay
		if newWS < 0 {
			newWS = 0
		}
		newWSU := uint64(newWS)
		var faults uint64
		if newWSU > s.ws {
			faults = (newWSU - s.ws) / 4096
		}
		// Network: cumulative bytes at the configured interface rate.
		rxBytes := uint64(u * float64(spec.NetRxBps) * secPerTick * float64(delta))
		txBytes := uint64(u * float64(spec.NetTxBps) * secPerTick * float64(delta))

		deltas = append(deltas, cDelta{
			id:         s.id,
			cpuNanos:   cpuNanos,
			cpuCores:   cpuCores,
			ws:         newWSU,
			usageBytes: uint64(float64(newWSU) * 1.2),
			rss:        uint64(float64(newWSU) * 0.7),
			faults:     faults,
			rxBytes:    rxBytes,
			txBytes:    txBytes,
			oom:        u >= 1.0,
		})
	}

	r.mu.Lock()
	for _, d := range deltas {
		c, ok := r.containers[d.id]
		if !ok {
			continue
		}
		c.usage.CPUCoreNanos += d.cpuNanos
		c.usage.CPUNanoCores = d.cpuCores
		c.usage.MemWorkingSet = d.ws
		c.usage.MemUsageBytes = d.usageBytes
		c.usage.MemRSS = d.rss
		c.usage.PageFaults += d.faults
		c.usage.NetRxBytes += d.rxBytes
		c.usage.NetTxBytes += d.txBytes
		if d.oom && r.opts.LimitEnforcement && c.state() == criv1.ContainerState_CONTAINER_RUNNING {
			if c.p != nil {
				c.p.State = criv1.ContainerState_CONTAINER_EXITED
				c.reconcile()
				c.s.FinishedAt = r.simTime().UnixNano()
				c.s.ExitCode = 137
				c.s.Reason = "OOMKilled"
				c.s.Message = "simulated out of memory: utilization reached the limit"
				r.emit(Event{
					Kind:    EventContainerStateChanged,
					ID:      d.id,
					Sandbox: c.p.PodSandboxId,
					FromC:   criv1.ContainerState_CONTAINER_RUNNING,
					ToC:     criv1.ContainerState_CONTAINER_EXITED,
					Reason:  "OOMKilled",
					At:      r.now,
				})
			}
		}
	}
	r.recomputeSandboxUsage()
	r.mu.Unlock()
}

// recomputeSandboxUsage re-aggregates each sandbox's usage from its member
// containers plus a small network overhead for the sandbox namespace. Call
// with the runtime lock held.
func (r *Runtime) recomputeSandboxUsage() {
	for _, sb := range r.sandboxes {
		if sb.p == nil {
			continue
		}
		var u Usage
		for _, c := range r.containers {
			if c.p != nil && c.p.PodSandboxId == sb.p.Id {
				u.CPUCoreNanos += c.usage.CPUCoreNanos
				u.CPUNanoCores += c.usage.CPUNanoCores
				u.MemUsageBytes += c.usage.MemUsageBytes
				u.MemWorkingSet += c.usage.MemWorkingSet
				u.MemRSS += c.usage.MemRSS
				u.PageFaults += c.usage.PageFaults
				u.NetRxBytes += c.usage.NetRxBytes
				u.NetTxBytes += c.usage.NetTxBytes
			}
		}
		// 10% network overhead for the sandbox's own namespace.
		u.NetRxBytes += u.NetRxBytes / 10
		u.NetTxBytes += u.NetTxBytes / 10
		sb.usage = u
	}
}

func u64p(v uint64) *criv1.UInt64Value { return &criv1.UInt64Value{Value: v} }

// materializeContainerStats builds a CRI ContainerStats from the internal
// usage. Call with the runtime lock held.
func (r *Runtime) materializeContainerStats(c *Container) *criv1.ContainerStats {
	if c == nil || c.p == nil {
		return nil
	}
	ts := r.simTime().UnixNano()
	return &criv1.ContainerStats{
		Attributes: &criv1.ContainerAttributes{
			Id:          c.p.Id,
			Metadata:    c.p.Metadata,
			Labels:      c.p.Labels,
			Annotations: c.p.Annotations,
		},
		Cpu: &criv1.CpuUsage{
			Timestamp:            ts,
			UsageCoreNanoSeconds: u64p(c.usage.CPUCoreNanos),
			UsageNanoCores:       u64p(c.usage.CPUNanoCores),
		},
		Memory: &criv1.MemoryUsage{
			Timestamp:       ts,
			WorkingSetBytes: u64p(c.usage.MemWorkingSet),
			UsageBytes:      u64p(c.usage.MemUsageBytes),
			RssBytes:        u64p(c.usage.MemRSS),
			PageFaults:      u64p(c.usage.PageFaults),
		},
	}
}

// materializePodSandboxStats builds a CRI PodSandboxStats from the sandbox's
// aggregated usage and its member containers. Call with the runtime lock
// held.
func (r *Runtime) materializePodSandboxStats(sb *Sandbox) *criv1.PodSandboxStats {
	if sb == nil || sb.p == nil {
		return nil
	}
	ts := r.simTime().UnixNano()
	stats := &criv1.PodSandboxStats{
		Attributes: &criv1.PodSandboxAttributes{
			Id:          sb.p.Id,
			Metadata:    sb.p.Metadata,
			Labels:      sb.p.Labels,
			Annotations: sb.p.Annotations,
		},
		Linux: &criv1.LinuxPodSandboxStats{
			Cpu: &criv1.CpuUsage{
				Timestamp:            ts,
				UsageCoreNanoSeconds: u64p(sb.usage.CPUCoreNanos),
				UsageNanoCores:       u64p(sb.usage.CPUNanoCores),
			},
			Memory: &criv1.MemoryUsage{
				Timestamp:       ts,
				WorkingSetBytes: u64p(sb.usage.MemWorkingSet),
				UsageBytes:      u64p(sb.usage.MemUsageBytes),
				RssBytes:        u64p(sb.usage.MemRSS),
				PageFaults:      u64p(sb.usage.PageFaults),
			},
			Network: &criv1.NetworkUsage{
				Timestamp: ts,
				DefaultInterface: &criv1.NetworkInterfaceUsage{
					Name:    "eth0",
					RxBytes: u64p(sb.usage.NetRxBytes),
					TxBytes: u64p(sb.usage.NetTxBytes),
				},
			},
		},
	}
	for _, c := range r.containers {
		if c.p != nil && c.p.PodSandboxId == sb.p.Id {
			stats.Linux.Containers = append(stats.Linux.Containers, r.materializeContainerStats(c))
		}
	}
	return stats
}

// metricDescriptors maps internal usage counters to CRI metric descriptors.
// Names are provisional (docs/cri-model-redesign.md, decision D1).
var metricDescriptors = []struct {
	name string
	typ  criv1.MetricType
	val  func(*Usage) uint64
}{
	{"cpu/usage", criv1.MetricType_COUNTER, func(u *Usage) uint64 { return u.CPUCoreNanos }},
	{"cpu/usage_cores", criv1.MetricType_GAUGE, func(u *Usage) uint64 { return u.CPUNanoCores }},
	{"memory/usage", criv1.MetricType_GAUGE, func(u *Usage) uint64 { return u.MemUsageBytes }},
	{"memory/working_set", criv1.MetricType_GAUGE, func(u *Usage) uint64 { return u.MemWorkingSet }},
	{"memory/rss", criv1.MetricType_GAUGE, func(u *Usage) uint64 { return u.MemRSS }},
	{"network/rx_bytes", criv1.MetricType_COUNTER, func(u *Usage) uint64 { return u.NetRxBytes }},
	{"network/tx_bytes", criv1.MetricType_COUNTER, func(u *Usage) uint64 { return u.NetTxBytes }},
}

func metricsFromUsage(u *Usage, ts int64) []*criv1.Metric {
	out := make([]*criv1.Metric, 0, len(metricDescriptors))
	for _, d := range metricDescriptors {
		out = append(out, &criv1.Metric{
			Name:       d.name,
			Timestamp:  ts,
			MetricType: d.typ,
			Value:      u64p(d.val(u)),
		})
	}
	return out
}

// PodSandboxMetrics materializes the metric descriptors for a sandbox and
// its member containers, or nil if the sandbox does not exist.
func (r *Runtime) PodSandboxMetrics(id string) *criv1.PodSandboxMetrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb, ok := r.sandboxes[id]
	if !ok {
		return nil
	}
	ts := r.simTime().UnixNano()
	m := &criv1.PodSandboxMetrics{
		PodSandboxId: id,
		Metrics:      metricsFromUsage(&sb.usage, ts),
	}
	for _, c := range r.containers {
		if c.p != nil && c.p.PodSandboxId == id {
			m.ContainerMetrics = append(m.ContainerMetrics, &criv1.ContainerMetrics{
				ContainerId: c.p.Id,
				Metrics:     metricsFromUsage(&c.usage, ts),
			})
		}
	}
	return m
}
