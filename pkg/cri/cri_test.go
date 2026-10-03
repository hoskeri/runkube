package cri

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Integration test driving the CRI RPCs against the model. Lifecycle RPCs
// (RunPodSandbox/StopPodSandbox/StartContainer/StopContainer) are
// synchronous: they block until the simulated transition completes, so the
// post-call state is asserted directly. The model's event simulation keeps
// running underneath; only the RPCs wait on it.
func TestBuiltinCRIRuntime(t *testing.T) {
	runtime, err := New()
	if err != nil {
		t.Fatalf("failed to create runtime: %v", err)
	}
	ctx := context.Background()

	// 1. Create container without image - should fail.
	_, err = runtime.CreateContainer(ctx, &criv1.CreateContainerRequest{
		PodSandboxId: "sb1",
		Config: &criv1.ContainerConfig{
			Metadata: &criv1.ContainerMetadata{Name: "c1"},
			Image:    &criv1.ImageSpec{Image: "nginx"},
		},
	})
	if err == nil {
		t.Fatal("expected error creating container without image, got nil")
	}

	// 2. Pull image: a synchronous RPC that waits for the simulated pull.
	start := time.Now()
	_, err = runtime.PullImage(ctx, &criv1.PullImageRequest{Image: &criv1.ImageSpec{Image: "nginx"}})
	if err != nil {
		t.Fatalf("failed to pull image: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("PullImage must wait for the simulated pull, completed in %v", elapsed)
	}

	// 3. Run pod sandbox: synchronous — the RPC blocks until READY.
	start = time.Now()
	resp, err := runtime.RunPodSandbox(ctx, &criv1.RunPodSandboxRequest{
		Config: &criv1.PodSandboxConfig{Metadata: &criv1.PodSandboxMetadata{Name: "p1", Uid: "uid1"}},
	})
	if err != nil {
		t.Fatalf("failed to run pod sandbox: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("RunPodSandbox must wait for the simulated start, completed in %v", elapsed)
	}
	sbid := resp.PodSandboxId
	if st, _ := sandboxState(runtime, sbid); st != criv1.PodSandboxState_SANDBOX_READY {
		t.Fatalf("sandbox must be READY after RunPodSandbox returns, got %v", st)
	}

	// 4. Create container; creation is immediate, so CREATED is observable.
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

	// 5. Start the container: synchronous — the RPC blocks until RUNNING.
	start = time.Now()
	_, err = runtime.StartContainer(ctx, &criv1.StartContainerRequest{ContainerId: scid})
	if err != nil {
		t.Fatalf("failed to start container: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("StartContainer must wait for the simulated start, completed in %v", elapsed)
	}
	if cs, err := runtime.ContainerStatus(ctx, &criv1.ContainerStatusRequest{ContainerId: scid}); err != nil {
		t.Fatalf("failed to get status: %v", err)
	} else if cs.GetStatus().GetState() != criv1.ContainerState_CONTAINER_RUNNING {
		t.Fatalf("container must be RUNNING after StartContainer returns, got %v", cs.GetStatus().GetState())
	}

	// 6. Stop the container: synchronous — the RPC blocks until EXITED.
	start = time.Now()
	_, err = runtime.StopContainer(ctx, &criv1.StopContainerRequest{ContainerId: scid})
	if err != nil {
		t.Fatalf("failed to stop container: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("StopContainer must wait for the simulated stop, completed in %v", elapsed)
	}
	if cs, err := runtime.ContainerStatus(ctx, &criv1.ContainerStatusRequest{ContainerId: scid}); err != nil {
		t.Fatalf("failed to get status after stop: %v", err)
	} else if cs.GetStatus().GetState() != criv1.ContainerState_CONTAINER_EXITED {
		t.Fatalf("container must be EXITED after StopContainer returns, got %v", cs.GetStatus().GetState())
	}

	// 7. A container that ran must have accumulated CPU usage.
	stats, err := runtime.ContainerStats(ctx, &criv1.ContainerStatsRequest{ContainerId: scid})
	if err != nil {
		t.Fatalf("failed to get container stats: %v", err)
	}
	if stats.GetStats().GetCpu().GetUsageCoreNanoSeconds().GetValue() == 0 {
		t.Error("expected non-zero CPU usage for a container that ran")
	}

	// 8. Stop the sandbox: synchronous — the RPC blocks until NOTREADY and
	// the child container is EXITED.
	start = time.Now()
	if _, err = runtime.StopPodSandbox(ctx, &criv1.StopPodSandboxRequest{PodSandboxId: sbid}); err != nil {
		t.Fatalf("failed to stop pod sandbox: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Errorf("StopPodSandbox must wait for the simulated stop, completed in %v", elapsed)
	}
	if st, _ := sandboxState(runtime, sbid); st != criv1.PodSandboxState_SANDBOX_NOTREADY {
		t.Fatalf("sandbox must be NOTREADY after StopPodSandbox returns, got %v", st)
	}
	if cs, err := runtime.ContainerStatus(ctx, &criv1.ContainerStatusRequest{ContainerId: scid}); err != nil {
		t.Fatalf("failed to get status after sandbox stop: %v", err)
	} else if cs.GetStatus().GetState() != criv1.ContainerState_CONTAINER_EXITED {
		t.Fatalf("child container must be EXITED after StopPodSandbox returns, got %v", cs.GetStatus().GetState())
	}
}

// TestSyncOpsHonorContext verifies that the synchronous RPCs abort with the
// right gRPC code when the caller's context is canceled or expires before
// the simulated transition completes.
func TestSyncOpsHonorContext(t *testing.T) {
	runtime, err := New()
	if err != nil {
		t.Fatalf("failed to create runtime: %v", err)
	}
	ctx := context.Background()

	_, err = runtime.PullImage(ctx, &criv1.PullImageRequest{Image: &criv1.ImageSpec{Image: "nginx"}})
	if err != nil {
		t.Fatalf("failed to pull image: %v", err)
	}
	resp, err := runtime.RunPodSandbox(ctx, &criv1.RunPodSandboxRequest{
		Config: &criv1.PodSandboxConfig{Metadata: &criv1.PodSandboxMetadata{Name: "p1", Uid: "uid1"}},
	})
	if err != nil {
		t.Fatalf("failed to run pod sandbox: %v", err)
	}
	cresp, err := runtime.CreateContainer(ctx, &criv1.CreateContainerRequest{
		PodSandboxId: resp.PodSandboxId,
		Config: &criv1.ContainerConfig{
			Metadata: &criv1.ContainerMetadata{Name: "c1"},
			Image:    &criv1.ImageSpec{Image: "nginx"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create container: %v", err)
	}
	scid := cresp.ContainerId

	// An already-canceled context aborts the synchronous wait immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = runtime.StartContainer(ctx, &criv1.StartContainerRequest{ContainerId: scid}); status.Code(err) != codes.Canceled {
		t.Fatalf("expected codes.Canceled for canceled context, got %v", err)
	}
	cancel()

	// A deadline that expires before the simulated start (10 ticks ≈ 1s at
	// the default 100ms tick) aborts with codes.DeadlineExceeded.
	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err = runtime.StartContainer(ctx, &criv1.StartContainerRequest{ContainerId: scid}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expected codes.DeadlineExceeded for expired deadline, got %v", err)
	}
}

func sandboxState(r *BuiltinCRIRuntime, sbid string) (criv1.PodSandboxState, bool) {
	s, err := r.PodSandboxStatus(context.Background(), &criv1.PodSandboxStatusRequest{PodSandboxId: sbid})
	if err != nil {
		return 0, false
	}
	return s.GetStatus().GetState(), true
}