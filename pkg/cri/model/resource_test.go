package model

import (
	"testing"
	"time"

	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func runningContainer(id, sbID string) *Container {
	return NewContainer(
		&criv1.Container{Id: id, PodSandboxId: sbID, State: criv1.ContainerState_CONTAINER_RUNNING},
		&criv1.ContainerStatus{Id: id, State: criv1.ContainerState_CONTAINER_RUNNING},
	)
}

// Defaults: 100ms tick, 100 millicores, load 0.35 while running. Over 100
// ticks: 0.35 * 100 * 1e6 ns/millicore-s * 0.1 s/tick * 100 ticks.
func TestDefaultAccountingAccumulates(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.Advance(100)

	stats, ok := r.GetContainerStats("c1")
	if !ok {
		t.Fatal("no stats")
	}
	wantCPU := uint64(0.35 * 100 * 1e6 * 0.1 * 100) // 350,000,000
	if got := stats.GetCpu().GetUsageCoreNanoSeconds().GetValue(); got != wantCPU {
		t.Errorf("usage core nanos = %d, want %d", got, wantCPU)
	}
	wantCores := uint64(0.35 * 100 * 1e6) // 35,000,000 nano cores
	if got := stats.GetCpu().GetUsageNanoCores().GetValue(); got != wantCores {
		t.Errorf("usage nano cores = %d, want %d", got, wantCores)
	}
	if ws := stats.GetMemory().GetWorkingSetBytes().GetValue(); ws == 0 {
		t.Error("working set must be non-zero after 100 running ticks")
	}
	if ws := stats.GetMemory().GetWorkingSetBytes().GetValue(); ws > uint64(DefaultResourceSpec().MemBytes) {
		t.Errorf("working set %d exceeds the memory limit", ws)
	}
	if rx := stats.GetMemory().GetPageFaults().GetValue(); rx == 0 {
		t.Error("page faults must accrue while memory grows")
	}
}

func TestStateDependentLoad(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	r.AddContainer("idle", NewContainer(
		&criv1.Container{Id: "idle", PodSandboxId: "sb1", State: criv1.ContainerState_CONTAINER_CREATED},
		&criv1.ContainerStatus{Id: "idle", State: criv1.ContainerState_CONTAINER_CREATED},
	))
	r.AddContainer("busy", runningContainer("busy", "sb1"))
	r.Advance(100)

	idle, _ := r.GetContainerStats("idle")
	busy, _ := r.GetContainerStats("busy")
	if idle.GetCpu().GetUsageCoreNanoSeconds().GetValue() >= busy.GetCpu().GetUsageCoreNanoSeconds().GetValue() {
		t.Errorf("idle container must consume less CPU than a running one: %d vs %d",
			idle.GetCpu().GetUsageCoreNanoSeconds().GetValue(),
			busy.GetCpu().GetUsageCoreNanoSeconds().GetValue())
	}
}

func TestLoadHookOverride(t *testing.T) {
	r := New(Options{
		Hooks: Hooks{
			Load: func(rt *Runtime, ref ObjectRef, now Ticks) float64 {
				if ref.ID != "c1" {
					t.Errorf("hook ref = %v, want c1", ref)
				}
				return 1.0
			},
		},
	})
	defer r.Close()
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.Advance(50)

	stats, _ := r.GetContainerStats("c1")
	want := uint64(1.0 * 100 * 1e6 * 0.1 * 50)
	if got := stats.GetCpu().GetUsageCoreNanoSeconds().GetValue(); got != want {
		t.Errorf("usage core nanos = %d, want %d (hook must override the default load)", got, want)
	}
}

func TestMemoryRamp(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.Advance(1)
	early, _ := r.GetContainerStats("c1")
	earlyWS := early.GetMemory().GetWorkingSetBytes().GetValue()
	r.Advance(99)
	late, _ := r.GetContainerStats("c1")
	lateWS := late.GetMemory().GetWorkingSetBytes().GetValue()
	if lateWS <= earlyWS {
		t.Errorf("working set must ramp up over time: early=%d late=%d", earlyWS, lateWS)
	}
	// After stopping, the working set must drain back toward zero.
	c, _ := r.GetContainer("c1")
	c.Container().State = criv1.ContainerState_CONTAINER_EXITED
	r.UpdateContainer("c1", c)
	r.Advance(200)
	drained, _ := r.GetContainerStats("c1")
	if ws := drained.GetMemory().GetWorkingSetBytes().GetValue(); ws >= lateWS {
		t.Errorf("working set must drain after stop: stopped=%d want < %d", ws, lateWS)
	}
}

func TestSandboxAggregation(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	r.AddSandbox("sb1", NewSandbox(
		&criv1.PodSandbox{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
		&criv1.PodSandboxStatus{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
	))
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.AddContainer("c2", runningContainer("c2", "sb1"))
	r.Advance(50)

	stats, ok := r.GetPodSandboxStats("sb1")
	if !ok {
		t.Fatal("no sandbox stats")
	}
	c1, _ := r.GetContainerStats("c1")
	c2, _ := r.GetContainerStats("c2")
	wantSum := c1.GetCpu().GetUsageCoreNanoSeconds().GetValue() + c2.GetCpu().GetUsageCoreNanoSeconds().GetValue()
	if got := stats.GetLinux().GetCpu().GetUsageCoreNanoSeconds().GetValue(); got != wantSum {
		t.Errorf("sandbox cpu = %d, want sum of containers %d", got, wantSum)
	}
	if len(stats.GetLinux().GetContainers()) != 2 {
		t.Errorf("sandbox stats must embed member container stats, got %d", len(stats.GetLinux().GetContainers()))
	}
	if rx := stats.GetLinux().GetNetwork().GetDefaultInterface().GetRxBytes().GetValue(); rx == 0 {
		t.Error("sandbox network rx must be non-zero")
	}
}

func TestMetricsDescriptors(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	r.AddSandbox("sb1", NewSandbox(
		&criv1.PodSandbox{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
		&criv1.PodSandboxStatus{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
	))
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.Advance(10)

	pm := r.PodSandboxMetrics("sb1")
	if pm == nil {
		t.Fatal("no metrics")
	}
	if pm.GetPodSandboxId() != "sb1" {
		t.Errorf("pod sandbox id = %q", pm.GetPodSandboxId())
	}
	byName := map[string]*criv1.Metric{}
	for _, m := range pm.GetMetrics() {
		byName[m.GetName()] = m
	}
	for _, name := range []string{"cpu/usage", "cpu/usage_cores", "memory/usage", "memory/working_set", "memory/rss", "network/rx_bytes", "network/tx_bytes"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing metric %q", name)
		}
	}
	if byName["cpu/usage"].GetMetricType() != criv1.MetricType_COUNTER {
		t.Errorf("cpu/usage must be a COUNTER, got %v", byName["cpu/usage"].GetMetricType())
	}
	if byName["memory/working_set"].GetMetricType() != criv1.MetricType_GAUGE {
		t.Errorf("memory/working_set must be a GAUGE, got %v", byName["memory/working_set"].GetMetricType())
	}
	if len(pm.GetContainerMetrics()) != 1 || pm.GetContainerMetrics()[0].GetContainerId() != "c1" {
		t.Errorf("container metrics missing, got %+v", pm.GetContainerMetrics())
	}
}

func TestLimitEnforcementOOM(t *testing.T) {
	newRuntime := func(enforce bool) *Runtime {
		return New(Options{
			Hooks: Hooks{
				Load: func(rt *Runtime, ref ObjectRef, now Ticks) float64 { return 2.0 },
			},
			LimitEnforcement: enforce,
		})
	}

	// Without enforcement the container stays running.
	r := newRuntime(false)
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.Advance(5)
	c, _ := r.GetContainer("c1")
	if c.state() != criv1.ContainerState_CONTAINER_RUNNING {
		t.Fatalf("without enforcement the container must keep running, got %v", c.state())
	}
	r.Close()

	// With enforcement it is OOM-killed.
	r = newRuntime(true)
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.Advance(5)
	c, _ = r.GetContainer("c1")
	if c.state() != criv1.ContainerState_CONTAINER_EXITED {
		t.Fatalf("with enforcement the container must exit, got %v", c.state())
	}
	if reason := c.ContainerStatus().GetReason(); reason != "OOMKilled" {
		t.Errorf("reason = %q, want OOMKilled", reason)
	}
	if code := c.ContainerStatus().GetExitCode(); code != 137 {
		t.Errorf("exit code = %d, want 137", code)
	}
	if ts := c.ContainerStatus().GetFinishedAt(); ts == 0 {
		t.Error("finished at must be set")
	}
	r.Close()
}

func TestDurationDefaultsAndHook(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	if got := r.Duration(OpImagePull); got != 10 {
		t.Errorf("default image pull duration = %d, want 10", got)
	}
	r2 := New(Options{
		Hooks: Hooks{
			Duration: func(kind OpKind) Ticks {
				if kind == OpImagePull {
					return 1
				}
				return 0
			},
		},
	})
	defer r2.Close()
	if got := r2.Duration(OpImagePull); got != 1 {
		t.Errorf("hooked image pull duration = %d, want 1", got)
	}
	if got := r2.Duration(OpSandboxStart); got != 0 {
		t.Errorf("hooked sandbox start duration = %d, want 0", got)
	}
}

func TestSetContainerSpec(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	spec := ResourceSpec{CPUMillis: 500, MemBytes: 1 << 30, NetRxBps: 1 << 10, NetTxBps: 1 << 10}
	if err := r.Do(SetContainerSpec("c1", spec)); err != nil {
		t.Fatalf("Do(SetContainerSpec): %v", err)
	}
	r.Advance(10)

	stats, _ := r.GetContainerStats("c1")
	want := uint64(0.35 * 500 * 1e6 * 0.1 * 10)
	if got := stats.GetCpu().GetUsageCoreNanoSeconds().GetValue(); got != want {
		t.Errorf("usage core nanos = %d, want %d (spec must be applied)", got, want)
	}
	if err := r.Do(SetContainerSpec("nope", spec)); err == nil {
		t.Error("SetContainerSpec for a missing container must fail")
	}
}

func TestDeterministicAccounting(t *testing.T) {
	run := func() uint64 {
		r := New(Options{})
		defer r.Close()
		r.AddContainer("c1", runningContainer("c1", "sb1"))
		r.Advance(37)
		r.Advance(63)
		stats, _ := r.GetContainerStats("c1")
		return stats.GetCpu().GetUsageCoreNanoSeconds().GetValue()
	}
	if a, b := run(), run(); a != b {
		t.Errorf("accounting must be deterministic: %d vs %d", a, b)
	}
}

func TestUsageAccessors(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	r.AddSandbox("sb1", NewSandbox(
		&criv1.PodSandbox{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
		&criv1.PodSandboxStatus{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
	))
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.Advance(10)
	sb, _ := r.GetSandbox("sb1")
	if sb.Usage().CPUCoreNanos == 0 {
		t.Error("sandbox usage must be populated")
	}
	c, _ := r.GetContainer("c1")
	if c.Usage().CPUCoreNanos == 0 {
		t.Error("container usage must be populated")
	}
}

func TestTickDurationConfig(t *testing.T) {
	r := New(Options{TickDuration: 1 * time.Second})
	defer r.Close()
	r.AddContainer("c1", runningContainer("c1", "sb1"))
	r.Advance(10)
	stats, _ := r.GetContainerStats("c1")
	want := uint64(0.35 * 100 * 1e6 * 1.0 * 10)
	if got := stats.GetCpu().GetUsageCoreNanoSeconds().GetValue(); got != want {
		t.Errorf("usage core nanos = %d, want %d (tick duration must scale byte/ns rates)", got, want)
	}
}
