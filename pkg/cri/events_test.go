package cri

import (
	"testing"

	"github.com/hoskeri/runkube/pkg/cri/model"
	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestContainerEventResponseMapping(t *testing.T) {
	rt := model.NewRuntime(0)
	defer rt.Close()

	sb := model.NewSandbox(
		&criv1.PodSandbox{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
		&criv1.PodSandboxStatus{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
	)
	rt.AddSandbox("sb1", sb)

	cases := []struct {
		name string
		ev   model.Event
		want criv1.ContainerEventType
	}{
		{"created", model.Event{Kind: model.EventContainerCreated, ID: "c1", Sandbox: "sb1"}, criv1.ContainerEventType_CONTAINER_CREATED_EVENT},
		{"started", model.Event{Kind: model.EventContainerStateChanged, ID: "c1", Sandbox: "sb1", ToC: criv1.ContainerState_CONTAINER_RUNNING}, criv1.ContainerEventType_CONTAINER_STARTED_EVENT},
		{"stopped", model.Event{Kind: model.EventContainerStateChanged, ID: "c1", Sandbox: "sb1", ToC: criv1.ContainerState_CONTAINER_EXITED}, criv1.ContainerEventType_CONTAINER_STOPPED_EVENT},
		{"deleted", model.Event{Kind: model.EventContainerRemoved, ID: "c1", Sandbox: "sb1"}, criv1.ContainerEventType_CONTAINER_DELETED_EVENT},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, ok := containerEventResponse(tc.ev, rt)
			if !ok {
				t.Fatalf("expected a mapped event for %+v", tc.ev)
			}
			if resp.GetContainerId() != "c1" {
				t.Errorf("container id = %q, want c1", resp.GetContainerId())
			}
			if resp.GetContainerEventType() != tc.want {
				t.Errorf("type = %v, want %v", resp.GetContainerEventType(), tc.want)
			}
			if resp.GetCreatedAt() == 0 {
				t.Error("created at must be set")
			}
			if ps := resp.GetPodSandboxStatus(); ps == nil || ps.GetId() != "sb1" {
				t.Errorf("pod sandbox status = %+v, want sb1 status", ps)
			}
		})
	}
}

func TestContainerEventResponseIgnoresNonContainerEvents(t *testing.T) {
	rt := model.NewRuntime(0)
	defer rt.Close()
	for _, ev := range []model.Event{
		{Kind: model.EventSandboxCreated, ID: "sb1"},
		{Kind: model.EventSandboxStateChanged, ID: "sb1"},
		{Kind: model.EventImagePulled, ID: "img"},
		{Kind: model.EventContainerStateChanged, ID: "c1", ToC: criv1.ContainerState_CONTAINER_CREATED},
	} {
		if _, ok := containerEventResponse(ev, rt); ok {
			t.Errorf("event %+v must not map to a container event", ev)
		}
	}
}

func TestContainerEventResponseAttachesStatusSnapshots(t *testing.T) {
	rt := model.NewRuntime(0)
	defer rt.Close()
	rt.AddSandbox("sb1", model.NewSandbox(
		&criv1.PodSandbox{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
		&criv1.PodSandboxStatus{Id: "sb1", State: criv1.PodSandboxState_SANDBOX_READY},
	))
	rt.AddContainer("c1", model.NewContainer(
		&criv1.Container{Id: "c1", PodSandboxId: "sb1", State: criv1.ContainerState_CONTAINER_RUNNING},
		&criv1.ContainerStatus{Id: "c1", State: criv1.ContainerState_CONTAINER_RUNNING, LogPath: "/var/log/c1"},
	))

	resp, ok := containerEventResponse(model.Event{
		Kind: model.EventContainerStateChanged, ID: "c1", Sandbox: "sb1", ToC: criv1.ContainerState_CONTAINER_RUNNING,
	}, rt)
	if !ok {
		t.Fatal("expected a mapped event")
	}
	if len(resp.GetContainersStatuses()) != 1 {
		t.Fatalf("container statuses = %d, want 1", len(resp.GetContainersStatuses()))
	}
	if lp := resp.GetContainersStatuses()[0].GetLogPath(); lp != "/var/log/c1" {
		t.Errorf("status log path = %q, want /var/log/c1", lp)
	}

	// After removal the status snapshot list must be empty, not stale.
	rt.RemoveContainer("c1")
	resp, ok = containerEventResponse(model.Event{Kind: model.EventContainerRemoved, ID: "c1", Sandbox: "sb1"}, rt)
	if !ok {
		t.Fatal("expected a mapped event")
	}
	if len(resp.GetContainersStatuses()) != 0 {
		t.Errorf("container statuses after removal = %d, want 0", len(resp.GetContainersStatuses()))
	}
}
