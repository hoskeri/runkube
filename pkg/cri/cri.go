package cri

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/hoskeri/runkube/pkg/cri/model"
	"github.com/hoskeri/runkube/pkg/netid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// BuiltinCRIRuntime is a simple in-memory implementation of the CRI.
type BuiltinCRIRuntime struct {
	criv1.UnimplementedImageServiceServer
	criv1.UnimplementedRuntimeServiceServer

	mu      sync.Mutex
	runtime *model.Runtime
}

// Version returns the runtime name and version information.
func (b *BuiltinCRIRuntime) Version(ctx context.Context, req *criv1.VersionRequest) (resp *criv1.VersionResponse, err error) {
	return &criv1.VersionResponse{
		Version:           "0.1.0",
		RuntimeName:       "breakbulk",
		RuntimeApiVersion: "v1",
		RuntimeVersion:    "1.0.0",
	}, nil
}

// UpdateRuntimeConfig updates the runtime configuration.
func (b *BuiltinCRIRuntime) UpdateRuntimeConfig(ctx context.Context, req *criv1.UpdateRuntimeConfigRequest) (resp *criv1.UpdateRuntimeConfigResponse, err error) {
	return &criv1.UpdateRuntimeConfigResponse{}, nil
}

// Status returns the status of the runtime.
func (b *BuiltinCRIRuntime) Status(ctx context.Context, req *criv1.StatusRequest) (resp *criv1.StatusResponse, err error) {
	return &criv1.StatusResponse{
		RuntimeHandlers: []*criv1.RuntimeHandler{
			{Name: "", Features: &criv1.RuntimeHandlerFeatures{RecursiveReadOnlyMounts: true, UserNamespaces: true}},
			{Name: "default", Features: &criv1.RuntimeHandlerFeatures{RecursiveReadOnlyMounts: true, UserNamespaces: true}},
		},
		Status: &criv1.RuntimeStatus{
			Conditions: []*criv1.RuntimeCondition{
				{Type: "RuntimeReady", Status: true},
				{Type: "NetworkReady", Status: true},
			},
		},
	}, nil
}

// ListMetricDescriptors lists the metric descriptors.
func (b *BuiltinCRIRuntime) ListMetricDescriptors(ctx context.Context, req *criv1.ListMetricDescriptorsRequest) (resp *criv1.ListMetricDescriptorsResponse, err error) {
	return &criv1.ListMetricDescriptorsResponse{}, nil
}

// RuntimeConfig returns the runtime configuration.
func (b *BuiltinCRIRuntime) RuntimeConfig(ctx context.Context, req *criv1.RuntimeConfigRequest) (resp *criv1.RuntimeConfigResponse, err error) {
	return &criv1.RuntimeConfigResponse{
		Linux: &criv1.LinuxRuntimeConfiguration{
			CgroupDriver: criv1.CgroupDriver_CGROUPFS,
		},
	}, nil
}

func (b *BuiltinCRIRuntime) uniqID(s ...string) string {
	return strings.Join(append(s, time.Now().UTC().Format("150405.000000")), "-")
}

// waitFuture blocks until the simulated event completes or ctx is done.
// The runtime lock must not be held while waiting. Context cancellation is
// translated into a gRPC status error so clients observe the right code.
func waitFuture(ctx context.Context, f *model.Future) error {
	if err := f.Wait(ctx); err != nil {
		switch ctx.Err() {
		case context.Canceled:
			return status.Errorf(codes.Canceled, "%v", err)
		case context.DeadlineExceeded:
			return status.Errorf(codes.DeadlineExceeded, "%v", err)
		}
		return err
	}
	return nil
}

// RunPodSandbox creates and starts a pod-level sandbox. The model performs
// the start asynchronously; the RPC blocks until the sandbox has reached
// READY so callers observe a fully started sandbox.
func (b *BuiltinCRIRuntime) RunPodSandbox(ctx context.Context, req *criv1.RunPodSandboxRequest) (resp *criv1.RunPodSandboxResponse, err error) {
	b.mu.Lock()
	if req.GetConfig().GetMetadata().GetUid() == "" {
		b.mu.Unlock()
		return nil, status.Errorf(codes.InvalidArgument, "pod sandbox id empty")
	}

	sbid := b.uniqID("sandbox", req.Config.GetMetadata().GetUid())
	_, ok := b.runtime.GetSandbox(sbid)
	if ok {
		b.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "%s already exists", sbid)
	}

	np := &criv1.PodSandbox{
		Id:          sbid,
		Labels:      req.GetConfig().GetLabels(),
		Annotations: req.GetConfig().GetAnnotations(),
		Metadata: &criv1.PodSandboxMetadata{
			Name:      req.GetConfig().GetMetadata().GetName(),
			Uid:       req.GetConfig().GetMetadata().GetUid(),
			Namespace: req.GetConfig().GetMetadata().GetNamespace(),
			Attempt:   req.GetConfig().GetMetadata().GetAttempt(),
		},
		RuntimeHandler: req.GetRuntimeHandler(),
		CreatedAt:      time.Now().UTC().UnixNano(),
		State:          criv1.PodSandboxState_SANDBOX_NOTREADY,
	}

	nps := &criv1.PodSandboxStatus{
		Id:             sbid,
		Metadata:       np.GetMetadata(),
		Labels:         np.GetLabels(),
		State:          np.GetState(),
		CreatedAt:      np.GetCreatedAt(),
		Annotations:    np.GetAnnotations(),
		RuntimeHandler: np.GetRuntimeHandler(),
		Network: &criv1.PodSandboxNetworkStatus{
			Ip: netid.FromMachineID(sbid).IPv4.String(),
		},
		Linux: &criv1.LinuxPodSandboxStatus{},
	}

	b.runtime.AddSandbox(sbid, model.NewSandbox(np, nps))

	// Schedule the READY transition in the model (async); the RPC blocks
	// until the sandbox has started.
	f := b.runtime.Schedule(b.runtime.Duration(model.OpSandboxStart), func(r *model.Runtime) {
		if sb, ok := r.GetSandbox(sbid); ok {
			sb.PodSandbox().State = criv1.PodSandboxState_SANDBOX_READY
			sb.PodSandboxStatus().State = criv1.PodSandboxState_SANDBOX_READY
			r.UpdateSandbox(sbid, sb)
		}
	})
	b.mu.Unlock()

	if err := waitFuture(ctx, f); err != nil {
		return nil, err
	}

	return &criv1.RunPodSandboxResponse{
		PodSandboxId: sbid,
	}, nil
}

// StopPodSandbox stops the sandbox. The model performs the stop
// asynchronously; the RPC blocks until the sandbox is NOTREADY and its
// child containers are EXITED.
func (b *BuiltinCRIRuntime) StopPodSandbox(ctx context.Context, req *criv1.StopPodSandboxRequest) (resp *criv1.StopPodSandboxResponse, err error) {
	b.mu.Lock()
	sbid := req.GetPodSandboxId()

	_, ok := b.runtime.GetSandbox(sbid)
	if !ok {
		b.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "%s not found", sbid)
	}

	// Schedule the NOTREADY transition and child stop in the model (async);
	// the RPC blocks until the sandbox has stopped.
	f := b.runtime.Schedule(b.runtime.Duration(model.OpSandboxStop), func(r *model.Runtime) {
		if sb, ok := r.GetSandbox(sbid); ok {
			sb.PodSandbox().State = criv1.PodSandboxState_SANDBOX_NOTREADY
			sb.PodSandboxStatus().State = criv1.PodSandboxState_SANDBOX_NOTREADY
			r.UpdateSandbox(sbid, sb)
		}
		// Transition child containers to EXITED
		for _, v := range r.ListContainers(nil) {
			if v.Container().PodSandboxId == sbid {
				v.Container().State = criv1.ContainerState_CONTAINER_EXITED
				v.ContainerStatus().State = criv1.ContainerState_CONTAINER_EXITED
				v.ContainerStatus().FinishedAt = time.Now().UTC().UnixNano()
				r.UpdateContainer(v.Container().Id, v)
			}
		}
	})
	b.mu.Unlock()

	if err := waitFuture(ctx, f); err != nil {
		return nil, err
	}

	return &criv1.StopPodSandboxResponse{}, nil
}

// RemovePodSandbox removes the sandbox.
func (b *BuiltinCRIRuntime) RemovePodSandbox(ctx context.Context, req *criv1.RemovePodSandboxRequest) (resp *criv1.RemovePodSandboxResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sbid := req.GetPodSandboxId()

	if sbid == "" {
		return nil, status.Errorf(codes.InvalidArgument, "pod sandbox id empty")
	}

	_, ok := b.runtime.GetSandbox(sbid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "%s not found", sbid)
	}

	for _, v := range b.runtime.ListContainers(nil) {
		if v.Container().PodSandboxId == sbid {
			return nil, status.Errorf(codes.InvalidArgument, "pod sandbox %q has containers", sbid)
		}
	}

	b.runtime.RemoveSandbox(sbid)
	return &criv1.RemovePodSandboxResponse{}, nil
}

// PodSandboxStatus returns the status of the sandbox.
func (b *BuiltinCRIRuntime) PodSandboxStatus(ctx context.Context, req *criv1.PodSandboxStatusRequest) (resp *criv1.PodSandboxStatusResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sbid := req.GetPodSandboxId()

	o, ok := b.runtime.GetSandbox(sbid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "%s not found", sbid)
	}

	return &criv1.PodSandboxStatusResponse{
		Status: proto.Clone(o.PodSandboxStatus()).(*criv1.PodSandboxStatus),
	}, nil
}

// ListPodSandbox returns a list of sandboxes.
func (b *BuiltinCRIRuntime) ListPodSandbox(ctx context.Context, req *criv1.ListPodSandboxRequest) (*criv1.ListPodSandboxResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	f := req.GetFilter()
	ss := []*criv1.PodSandbox{}
	for _, x := range b.runtime.ListSandboxes(f) {
		ss = append(ss, proto.Clone(x.PodSandbox()).(*criv1.PodSandbox))
	}

	return &criv1.ListPodSandboxResponse{Items: ss}, nil
}

// PodSandboxStats returns stats of the sandbox.
func (b *BuiltinCRIRuntime) PodSandboxStats(ctx context.Context, req *criv1.PodSandboxStatsRequest) (resp *criv1.PodSandboxStatsResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sbid := req.GetPodSandboxId()
	stats, ok := b.runtime.GetPodSandboxStats(sbid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "stats for sandbox %s not found", sbid)
	}
	return &criv1.PodSandboxStatsResponse{Stats: stats}, nil
}

// UpdatePodSandboxResources updates resources of the sandbox.
func (b *BuiltinCRIRuntime) UpdatePodSandboxResources(ctx context.Context, req *criv1.UpdatePodSandboxResourcesRequest) (resp *criv1.UpdatePodSandboxResourcesResponse, err error) {
	return nil, nil
}

// ListPodSandboxStats returns stats of the sandboxes.
func (b *BuiltinCRIRuntime) ListPodSandboxStats(ctx context.Context, req *criv1.ListPodSandboxStatsRequest) (resp *criv1.ListPodSandboxStatsResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.runtime.ListPodSandboxStats(req.GetFilter())
	return &criv1.ListPodSandboxStatsResponse{Stats: list}, nil
}

// ListPodSandboxMetrics returns the metric descriptors for all sandboxes.
func (b *BuiltinCRIRuntime) ListPodSandboxMetrics(ctx context.Context, req *criv1.ListPodSandboxMetricsRequest) (resp *criv1.ListPodSandboxMetricsResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*criv1.PodSandboxMetrics
	for _, sb := range b.runtime.ListSandboxes(nil) {
		if m := b.runtime.PodSandboxMetrics(sb.PodSandbox().GetId()); m != nil {
			out = append(out, m)
		}
	}
	return &criv1.ListPodSandboxMetricsResponse{PodMetrics: out}, nil
}

// CreateContainer creates a new container in specified PodSandbox.
func (b *BuiltinCRIRuntime) CreateContainer(ctx context.Context, req *criv1.CreateContainerRequest) (resp *criv1.CreateContainerResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sbid := req.GetPodSandboxId()
	if sbid == "" {
		return nil, status.Errorf(codes.InvalidArgument, "container sandbox id is empty")
	}

	_, ok := b.runtime.GetSandbox(sbid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "%s not found", sbid)
	}

	imgName := req.GetConfig().GetImage().GetImage()
	_, imagePulled := b.runtime.GetImage(imgName)
	if !imagePulled {
		return nil, status.Errorf(codes.NotFound, "image %s not found", imgName)
	}

	scid := b.uniqID("container", sbid, req.GetConfig().GetMetadata().GetName())

	_, ok = b.runtime.GetContainer(scid)
	if ok {
		return nil, status.Errorf(codes.AlreadyExists, "container %s already exists", scid)
	}

	nc := &criv1.Container{
		Id:           scid,
		PodSandboxId: sbid,
		Metadata:     req.GetConfig().GetMetadata(),
		Image:        req.GetConfig().GetImage(),
		ImageRef:     req.GetConfig().GetImage().Image,
		State:        criv1.ContainerState_CONTAINER_CREATED,
		CreatedAt:    time.Now().UTC().UnixNano(),
		Labels:       req.GetConfig().GetLabels(),
		Annotations:  req.GetConfig().GetAnnotations(),
		ImageId:      req.GetConfig().GetImage().GetImage(),
	}

	ncs := &criv1.ContainerStatus{
		Id:          scid,
		State:       nc.GetState(),
		Metadata:    nc.GetMetadata(),
		CreatedAt:   nc.GetCreatedAt(),
		Image:       nc.GetImage(),
		ImageRef:    nc.GetImage().Image,
		Labels:      nc.GetLabels(),
		Annotations: nc.GetAnnotations(),
		LogPath:     req.GetConfig().GetLogPath(),
	}

	b.runtime.AddContainer(scid, model.NewContainer(nc, ncs))

	return &criv1.CreateContainerResponse{
		ContainerId: scid,
	}, nil
}

// StartContainer starts the container. The model performs the start
// asynchronously; the RPC blocks until the container has reached RUNNING.
func (b *BuiltinCRIRuntime) StartContainer(ctx context.Context, req *criv1.StartContainerRequest) (resp *criv1.StartContainerResponse, err error) {
	b.mu.Lock()
	scid := req.GetContainerId()
	_, ok := b.runtime.GetContainer(scid)
	if !ok {
		b.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "container %s not found", scid)
	}

	// Schedule the RUNNING transition in the model (async); the RPC blocks
	// until the container has started.
	f := b.runtime.Schedule(b.runtime.Duration(model.OpContainerStart), func(r *model.Runtime) {
		if c, ok := r.GetContainer(scid); ok {
			c.Container().State = criv1.ContainerState_CONTAINER_RUNNING
			c.ContainerStatus().State = criv1.ContainerState_CONTAINER_RUNNING
			c.ContainerStatus().StartedAt = time.Now().UTC().UnixNano()
			r.UpdateContainer(scid, c)
		}
	})
	b.mu.Unlock()

	if err := waitFuture(ctx, f); err != nil {
		return nil, err
	}

	return &criv1.StartContainerResponse{}, nil
}

// StopContainer stops a running container with a grace period (i.e.,
// timeout). The model performs the stop asynchronously; the RPC blocks
// until the container has reached EXITED.
func (b *BuiltinCRIRuntime) StopContainer(ctx context.Context, req *criv1.StopContainerRequest) (resp *criv1.StopContainerResponse, err error) {
	b.mu.Lock()
	scid := req.GetContainerId()
	_, ok := b.runtime.GetContainer(scid)
	if !ok {
		b.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "%s not found", scid)
	}

	// Schedule the EXITED transition in the model (async); the RPC blocks
	// until the container has stopped.
	f := b.runtime.Schedule(b.runtime.Duration(model.OpContainerStop), func(r *model.Runtime) {
		if c, ok := r.GetContainer(scid); ok {
			c.Container().State = criv1.ContainerState_CONTAINER_EXITED
			c.ContainerStatus().State = criv1.ContainerState_CONTAINER_EXITED
			c.ContainerStatus().FinishedAt = time.Now().UTC().UnixNano()
			r.UpdateContainer(scid, c)
		}
	})
	b.mu.Unlock()

	if err := waitFuture(ctx, f); err != nil {
		return nil, err
	}

	return &criv1.StopContainerResponse{}, nil
}

// RemoveContainer removes the container.
func (b *BuiltinCRIRuntime) RemoveContainer(ctx context.Context, req *criv1.RemoveContainerRequest) (resp *criv1.RemoveContainerResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	scid := req.GetContainerId()

	_, ok := b.runtime.GetContainer(scid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "%s not found", scid)
	}

	b.runtime.RemoveContainer(scid)
	return &criv1.RemoveContainerResponse{}, nil
}

// ListContainers lists all containers by filters.
func (b *BuiltinCRIRuntime) ListContainers(ctx context.Context, req *criv1.ListContainersRequest) (resp *criv1.ListContainersResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	f := req.GetFilter()
	ss := []*criv1.Container{}
	for _, x := range b.runtime.ListContainers(f) {
		ss = append(ss, proto.Clone(x.Container()).(*criv1.Container))
	}
	return &criv1.ListContainersResponse{
		Containers: ss,
	}, nil
}

// ContainerStatus returns the status of the container.
func (b *BuiltinCRIRuntime) ContainerStatus(ctx context.Context, req *criv1.ContainerStatusRequest) (resp *criv1.ContainerStatusResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	scid := req.GetContainerId()
	o, ok := b.runtime.GetContainer(scid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "%s not found", scid)
	}

	return &criv1.ContainerStatusResponse{Status: proto.Clone(o.ContainerStatus()).(*criv1.ContainerStatus)}, nil
}

// UpdateContainerResources updates resources of the container.
func (b *BuiltinCRIRuntime) UpdateContainerResources(ctx context.Context, req *criv1.UpdateContainerResourcesRequest) (resp *criv1.UpdateContainerResourcesResponse, err error) {
	return nil, nil
}

// ReopenContainerLog reopens the container log.
func (b *BuiltinCRIRuntime) ReopenContainerLog(ctx context.Context, req *criv1.ReopenContainerLogRequest) (resp *criv1.ReopenContainerLogResponse, err error) {
	return nil, nil
}

// ExecSync executes a command in the container synchronously.
func (b *BuiltinCRIRuntime) ExecSync(ctx context.Context, req *criv1.ExecSyncRequest) (resp *criv1.ExecSyncResponse, err error) {
	return nil, nil
}

// Exec executes a command in the container.
func (b *BuiltinCRIRuntime) Exec(ctx context.Context, req *criv1.ExecRequest) (resp *criv1.ExecResponse, err error) {
	return nil, nil
}

// Attach attaches to a running container.
func (b *BuiltinCRIRuntime) Attach(ctx context.Context, req *criv1.AttachRequest) (resp *criv1.AttachResponse, err error) {
	return nil, nil
}

// PortForward forwards a port from a pod.
func (b *BuiltinCRIRuntime) PortForward(ctx context.Context, req *criv1.PortForwardRequest) (resp *criv1.PortForwardResponse, err error) {
	return nil, nil
}

// ContainerStats returns stats of the container.
func (b *BuiltinCRIRuntime) ContainerStats(ctx context.Context, req *criv1.ContainerStatsRequest) (resp *criv1.ContainerStatsResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	scid := req.GetContainerId()
	stats, ok := b.runtime.GetContainerStats(scid)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "stats for container %s not found", scid)
	}
	return &criv1.ContainerStatsResponse{Stats: stats}, nil
}

// ListContainerStats returns stats of the containers.
func (b *BuiltinCRIRuntime) ListContainerStats(ctx context.Context, req *criv1.ListContainerStatsRequest) (resp *criv1.ListContainerStatsResponse, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.runtime.ListContainerStats(req.GetFilter())
	return &criv1.ListContainerStatsResponse{Stats: list}, nil
}

// CheckpointContainer checkpoints a container.
func (b *BuiltinCRIRuntime) CheckpointContainer(ctx context.Context, req *criv1.CheckpointContainerRequest) (resp *criv1.CheckpointContainerResponse, err error) {
	return nil, nil
}

// GetContainerEvents streams container lifecycle events from the model's
// event stream, mapped to CRI container event types.
func (b *BuiltinCRIRuntime) GetContainerEvents(req *criv1.GetEventsRequest, srv criv1.RuntimeService_GetContainerEventsServer) error {
	sub := b.runtime.Subscribe()
	defer sub.Close()
	for ev := range sub.Events() {
		resp, ok := containerEventResponse(ev, b.runtime)
		if !ok {
			continue
		}
		if err := srv.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// streamMetricsInterval is the sampling period for StreamPodSandboxMetrics.
const streamMetricsInterval = time.Second

// StreamPodSandboxMetrics streams periodic metric samples for all sandboxes.
func (b *BuiltinCRIRuntime) StreamPodSandboxMetrics(req *criv1.StreamPodSandboxMetricsRequest, srv criv1.RuntimeService_StreamPodSandboxMetricsServer) error {
	ticker := time.NewTicker(streamMetricsInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			b.mu.Lock()
			var out []*criv1.PodSandboxMetrics
			for _, sb := range b.runtime.ListSandboxes(nil) {
				if m := b.runtime.PodSandboxMetrics(sb.PodSandbox().GetId()); m != nil {
					out = append(out, m)
				}
			}
			b.mu.Unlock()
			if err := srv.Send(&criv1.StreamPodSandboxMetricsResponse{PodSandboxMetrics: out}); err != nil {
				return err
			}
		case <-srv.Context().Done():
			return srv.Context().Err()
		}
	}
}

// containerEventResponse maps a model event to a CRI container event, or ok
// == false when the event is not a container lifecycle event. Status
// snapshots are attached when the objects still exist.
func containerEventResponse(ev model.Event, rt *model.Runtime) (resp *criv1.ContainerEventResponse, ok bool) {
	var typ criv1.ContainerEventType
	switch ev.Kind {
	case model.EventContainerCreated:
		typ = criv1.ContainerEventType_CONTAINER_CREATED_EVENT
	case model.EventContainerStateChanged:
		switch ev.ToC {
		case criv1.ContainerState_CONTAINER_RUNNING:
			typ = criv1.ContainerEventType_CONTAINER_STARTED_EVENT
		case criv1.ContainerState_CONTAINER_EXITED:
			typ = criv1.ContainerEventType_CONTAINER_STOPPED_EVENT
		default:
			return nil, false
		}
	case model.EventContainerRemoved:
		typ = criv1.ContainerEventType_CONTAINER_DELETED_EVENT
	default:
		return nil, false
	}

	out := &criv1.ContainerEventResponse{
		ContainerId:        ev.ID,
		ContainerEventType: typ,
		CreatedAt:          time.Now().UTC().UnixNano(),
	}
	if sb, ok := rt.GetSandbox(ev.Sandbox); ok && sb.PodSandboxStatus() != nil {
		out.PodSandboxStatus = proto.Clone(sb.PodSandboxStatus()).(*criv1.PodSandboxStatus)
	}
	if c, ok := rt.GetContainer(ev.ID); ok && c.ContainerStatus() != nil {
		out.ContainersStatuses = []*criv1.ContainerStatus{proto.Clone(c.ContainerStatus()).(*criv1.ContainerStatus)}
	}
	return out, true
}

// ListImages lists all images.
func (b *BuiltinCRIRuntime) ListImages(context.Context, *criv1.ListImagesRequest) (*criv1.ListImagesResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ss := []*criv1.Image{}
	for _, x := range b.runtime.ListImages(nil) {
		ss = append(ss, &criv1.Image{
			Id:   x.ImageSpec().GetImage(),
			Size: 1,
		})
	}
	return &criv1.ListImagesResponse{Images: ss}, nil
}

// ImageStatus returns the status of the image.
func (b *BuiltinCRIRuntime) ImageStatus(_ context.Context, req *criv1.ImageStatusRequest) (*criv1.ImageStatusResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	imgName := req.Image.GetImage()
	o, ok := b.runtime.GetImage(imgName)
	if !ok {
		return &criv1.ImageStatusResponse{}, nil
	}

	return &criv1.ImageStatusResponse{
		Image: &criv1.Image{
			Id:   o.ImageSpec().GetImage(),
			Size: 1,
		},
	}, nil
}

// PullImage pulls an image. Images are pulled asynchronously in the model;
// the RPC waits (synchronously) for the pull to complete.
func (b *BuiltinCRIRuntime) PullImage(ctx context.Context, req *criv1.PullImageRequest) (*criv1.PullImageResponse, error) {
	b.mu.Lock()
	f := b.runtime.Schedule(b.runtime.Duration(model.OpImagePull), func(r *model.Runtime) {
		r.AddImage(req.Image.GetImage(), model.NewImage(req.Image))
	})
	b.mu.Unlock()

	if err := waitFuture(ctx, f); err != nil {
		return nil, err
	}

	return &criv1.PullImageResponse{
		ImageRef: req.Image.GetImage(),
	}, nil
}

// RemoveImage removes the image.
func (b *BuiltinCRIRuntime) RemoveImage(ctx context.Context, req *criv1.RemoveImageRequest) (*criv1.RemoveImageResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.runtime.RemoveImage(req.Image.GetImage())
	return &criv1.RemoveImageResponse{}, nil
}

// ImageFsInfo returns information about the image filesystem.
func (b *BuiltinCRIRuntime) ImageFsInfo(context.Context, *criv1.ImageFsInfoRequest) (*criv1.ImageFsInfoResponse, error) {
	return &criv1.ImageFsInfoResponse{
		ImageFilesystems: []*criv1.FilesystemUsage{
			{
				FsId: &criv1.FilesystemIdentifier{
					Mountpoint: "/tmp",
				},
				UsedBytes: &criv1.UInt64Value{
					Value: 1,
				},
				Timestamp: time.Now().UTC().UnixNano(),
			},
		},
	}, nil
}

// New returns a new BuiltinCRIRuntime.
func New() (*BuiltinCRIRuntime, error) {
	return &BuiltinCRIRuntime{
		runtime: model.NewRuntime(100 * time.Millisecond),
	}, nil
}

// Register registers the runtime and image services with the gRPC server.
func Register(s *grpc.Server, b *BuiltinCRIRuntime) error {
	criv1.RegisterRuntimeServiceServer(s, b)
	criv1.RegisterImageServiceServer(s, b)
	return nil
}

var _ criv1.RuntimeServiceServer = &BuiltinCRIRuntime{}
var _ criv1.ImageServiceServer = &BuiltinCRIRuntime{}
