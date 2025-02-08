package cri

import (
	"context"
	"testing"
	"time"

	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func TestBuiltinCRIRuntime(t *testing.T) {
	runtime, err := New()
	if err != nil {
		t.Fatalf("failed to create runtime: %v", err)
	}

	ctx := context.Background()

	// 1. Try to create container without image - should fail
	_, err = runtime.CreateContainer(ctx, &criv1.CreateContainerRequest{
		PodSandboxId: "sb1",
		Config: &criv1.ContainerConfig{
			Metadata: &criv1.ContainerMetadata{Name: "c1"},
			Image:    &criv1.ImageSpec{Image: "nginx"},
		},
	})
	if err == nil {
		t.Fatal("expected error creating container without image, but got nil")
	}

	// 2. Pull image - should take some time
	start := time.Now()
	_, err = runtime.PullImage(ctx, &criv1.PullImageRequest{
		Image: &criv1.ImageSpec{Image: "nginx"},
	})
	if err != nil {
		t.Fatalf("failed to pull image: %v", err)
	}
	if time.Since(start) < 1*time.Second {
		t.Errorf("PullImage returned too quickly: %v", time.Since(start))
	}

	// 3. Run Pod Sandbox
	resp, err := runtime.RunPodSandbox(ctx, &criv1.RunPodSandboxRequest{
		Config: &criv1.PodSandboxConfig{
			Metadata: &criv1.PodSandboxMetadata{Name: "p1", Uid: "uid1"},
		},
	})
	if err != nil {
		t.Fatalf("failed to run pod sandbox: %v", err)
	}
	sbid := resp.PodSandboxId

	// Verify initial state is NOTREADY
	sresp, err := runtime.PodSandboxStatus(ctx, &criv1.PodSandboxStatusRequest{PodSandboxId: sbid})
	if err != nil {
		t.Fatalf("failed to get sandbox status: %v", err)
	}
	if sresp.Status.State != criv1.PodSandboxState_SANDBOX_NOTREADY {
		t.Errorf("expected initial state NOTREADY, got %v", sresp.Status.State)
	}

	// Wait for transition
	time.Sleep(1100 * time.Millisecond)
	sresp, err = runtime.PodSandboxStatus(ctx, &criv1.PodSandboxStatusRequest{PodSandboxId: sbid})
	if err != nil {
		t.Fatalf("failed to get sandbox status: %v", err)
	}
	if sresp.Status.State != criv1.PodSandboxState_SANDBOX_READY {
		t.Errorf("expected state READY after delay, got %v", sresp.Status.State)
	}

	// 4. Create Container
	cresp, err := runtime.CreateContainer(ctx, &criv1.CreateContainerRequest{
		PodSandboxId: sbid,
		Config: &criv1.ContainerConfig{
			Metadata: &criv1.ContainerMetadata{Name: "c1"},
			Image:    &criv1.ImageSpec{Image: "nginx"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create container: %v", err)
	}
	scid := cresp.ContainerId

	// 5. Start Container
	_, err = runtime.StartContainer(ctx, &criv1.StartContainerRequest{ContainerId: scid})
	if err != nil {
		t.Fatalf("failed to start container: %v", err)
	}

	// Verify initial state is CREATED
	cstatus, err := runtime.ContainerStatus(ctx, &criv1.ContainerStatusRequest{ContainerId: scid})
	if err != nil {
		t.Fatalf("failed to get container status: %v", err)
	}
	if cstatus.Status.State != criv1.ContainerState_CONTAINER_CREATED {
		t.Errorf("expected initial state CREATED, got %v", cstatus.Status.State)
	}

	// Wait for transition
	time.Sleep(1100 * time.Millisecond)
	cstatus, err = runtime.ContainerStatus(ctx, &criv1.ContainerStatusRequest{ContainerId: scid})
	if err != nil {
		t.Fatalf("failed to get container status: %v", err)
	}
	if cstatus.Status.State != criv1.ContainerState_CONTAINER_RUNNING {
		t.Errorf("expected state RUNNING after delay, got %v", cstatus.Status.State)
	}
}
