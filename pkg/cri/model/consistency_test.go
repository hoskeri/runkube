package model

import (
	"testing"

	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// The two protos (PodSandbox/PodSandboxStatus) carry a duplicated State
// field. Phase 2 makes the object proto authoritative: every write path
// (Add/Update) reconciles the status State to match, so the two can never
// report different states. This file tests the invariant.

func TestSandboxStateReconciledOnAdd(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	// Deliberately mismatched: p says NOTREADY, status says READY.
	r.AddSandbox("sb1", NewSandbox(
		&criv1.PodSandbox{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_NOTREADY},
		&criv1.PodSandboxStatus{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY,
			Network: &criv1.PodSandboxNetworkStatus{Ip: "10.2.3.4"}},
	))

	got, ok := r.GetSandbox("sb1")
	if !ok {
		t.Fatal("sandbox not found")
	}
	if got.PodSandbox().State != criv1.PodSandboxState_SANDBOX_NOTREADY {
		t.Errorf("PodSandbox.State = %v, want NOTREADY", got.PodSandbox().State)
	}
	if got.PodSandboxStatus().State != criv1.PodSandboxState_SANDBOX_NOTREADY {
		t.Errorf("PodSandboxStatus.State = %v, want NOTREADY (reconciled from PodSandbox)", got.PodSandboxStatus().State)
	}
	if ip := got.PodSandboxStatus().GetNetwork().GetIp(); ip != "10.2.3.4" {
		t.Errorf("status-only fields must survive reconciliation, ip = %q", ip)
	}
}

func TestSandboxTransitionViaPodState(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	r.AddSandbox("sb1", NewSandbox(
		&criv1.PodSandbox{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_NOTREADY},
		&criv1.PodSandboxStatus{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_NOTREADY},
	))

	// The cri.go transition pattern: mutate the PodSandbox clone and Update.
	sb, _ := r.GetSandbox("sb1")
	sb.PodSandbox().State = criv1.PodSandboxState_SANDBOX_READY
	sb.PodSandboxStatus().State = criv1.PodSandboxState_SANDBOX_READY
	r.UpdateSandbox("sb1", sb)

	got, _ := r.GetSandbox("sb1")
	if got.PodSandbox().State != criv1.PodSandboxState_SANDBOX_READY ||
		got.PodSandboxStatus().State != criv1.PodSandboxState_SANDBOX_READY {
		t.Errorf("states must agree after update: p=%v s=%v",
			got.PodSandbox().State, got.PodSandboxStatus().State)
	}

	// Status-only fields written by a transition must survive.
	got.PodSandboxStatus().Network = &criv1.PodSandboxNetworkStatus{Ip: "10.0.0.2"}
	r.UpdateSandbox("sb1", got)
	got2, _ := r.GetSandbox("sb1")
	if ip := got2.PodSandboxStatus().GetNetwork().GetIp(); ip != "10.0.0.2" {
		t.Errorf("status-only field must survive update, ip = %q", ip)
	}
	if got2.PodSandboxStatus().State != criv1.PodSandboxState_SANDBOX_READY {
		t.Error("state must survive update")
	}
}

func TestSandboxMatchUsesAuthoritativeState(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	r.AddSandbox("sb1", NewSandbox(
		&criv1.PodSandbox{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_NOTREADY},
		&criv1.PodSandboxStatus{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
	))
	list := r.ListSandboxes(&criv1.PodSandboxFilter{
		State: &criv1.PodSandboxStateValue{State: criv1.PodSandboxState_SANDBOX_NOTREADY},
	})
	if len(list) != 1 {
		t.Errorf("match must use the authoritative PodSandbox state, got %d results", len(list))
	}
}

func TestContainerStateReconciledOnAdd(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	r.AddContainer("c1", NewContainer(
		&criv1.Container{Id: "c1", PodSandboxId: "sb1", State: criv1.ContainerState_CONTAINER_CREATED},
		&criv1.ContainerStatus{Id: "c1", State: criv1.ContainerState_CONTAINER_RUNNING, LogPath: "/var/log/c1"},
	))

	c, ok := r.GetContainer("c1")
	if !ok {
		t.Fatal("container not found")
	}
	if c.Container().State != criv1.ContainerState_CONTAINER_CREATED {
		t.Errorf("Container.State = %v, want CREATED", c.Container().State)
	}
	if c.ContainerStatus().State != criv1.ContainerState_CONTAINER_CREATED {
		t.Errorf("ContainerStatus.State = %v, want CREATED (reconciled)", c.ContainerStatus().State)
	}
	if lp := c.ContainerStatus().GetLogPath(); lp != "/var/log/c1" {
		t.Errorf("status-only field must survive reconciliation, log path = %q", lp)
	}
}

func TestContainerTransitionKeepsFinishedAt(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	r.AddContainer("c1", NewContainer(
		&criv1.Container{Id: "c1", PodSandboxId: "sb1", State: criv1.ContainerState_CONTAINER_RUNNING},
		&criv1.ContainerStatus{Id: "c1", State: criv1.ContainerState_CONTAINER_RUNNING},
	))

	c, _ := r.GetContainer("c1")
	c.Container().State = criv1.ContainerState_CONTAINER_EXITED
	c.ContainerStatus().State = criv1.ContainerState_CONTAINER_EXITED
	c.ContainerStatus().FinishedAt = 12345
	r.UpdateContainer("c1", c)

	got, _ := r.GetContainer("c1")
	if got.ContainerStatus().State != criv1.ContainerState_CONTAINER_EXITED {
		t.Errorf("state = %v, want EXITED", got.ContainerStatus().State)
	}
	if got.ContainerStatus().FinishedAt != 12345 {
		t.Errorf("FinishedAt must survive the transition, got %d", got.ContainerStatus().FinishedAt)
	}
}

func TestContainerMatchUsesAuthoritativeState(t *testing.T) {
	r := NewRuntime(0)
	defer r.Close()
	r.AddContainer("c1", NewContainer(
		&criv1.Container{Id: "c1", PodSandboxId: "sb1", State: criv1.ContainerState_CONTAINER_RUNNING},
		&criv1.ContainerStatus{Id: "c1", State: criv1.ContainerState_CONTAINER_CREATED},
	))
	list := r.ListContainers(&criv1.ContainerFilter{
		State: &criv1.ContainerStateValue{State: criv1.ContainerState_CONTAINER_RUNNING},
	})
	if len(list) != 1 {
		t.Errorf("match must use the authoritative Container state, got %d results", len(list))
	}
}
