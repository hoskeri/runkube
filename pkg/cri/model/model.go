package model

import (
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/labels"
	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Runtime tracks the objects in container runtime discrete event model.
type Runtime struct {
	mu         sync.Mutex
	sandboxes  map[string]*Sandbox
	containers map[string]*Container
	images     map[string]*Image

	// Simulation clock (see engine.go).
	now   Ticks
	order uint64
	queue eventHeap

	// Optional wall-clock adapter.
	stopCh    chan struct{}
	closeOnce sync.Once

	// Resource accounting configuration (see resource.go).
	opts      Options
	startTime time.Time

	// Event subscriptions (see events.go).
	subs   map[uint64]*Subscription
	subSeq uint64
}

// Sandbox model
type Sandbox struct {
	p     *criv1.PodSandbox
	s     *criv1.PodSandboxStatus
	usage Usage
}

// Container model
type Container struct {
	p     *criv1.Container
	s     *criv1.ContainerStatus
	spec  ResourceSpec
	usage Usage
}

// Image model
type Image struct {
	i *criv1.ImageSpec
}

func NewSandbox(p *criv1.PodSandbox, s *criv1.PodSandboxStatus) *Sandbox {
	return &Sandbox{p: p, s: s}
}

func NewContainer(p *criv1.Container, s *criv1.ContainerStatus) *Container {
	return &Container{p: p, s: s}
}

func NewImage(i *criv1.ImageSpec) *Image {
	return &Image{i: i}
}

func (s *Sandbox) PodSandbox() *criv1.PodSandbox {
	if s == nil {
		return nil
	}
	return s.p
}

func (s *Sandbox) PodSandboxStatus() *criv1.PodSandboxStatus {
	if s == nil {
		return nil
	}
	return s.s
}

// Usage returns the sandbox's accumulated resource usage, aggregated from
// its member containers plus the sandbox namespace's network overhead.
func (s *Sandbox) Usage() Usage {
	if s == nil {
		return Usage{}
	}
	return s.usage
}

// state returns the authoritative sandbox state: the PodSandbox State when
// present, falling back to the status. The two are kept equal by reconcile.
func (s *Sandbox) state() criv1.PodSandboxState {
	if s != nil && s.p != nil {
		return s.p.State
	}
	if s != nil && s.s != nil {
		return s.s.State
	}
	return criv1.PodSandboxState_SANDBOX_NOTREADY
}

// reconcile keeps the status State in lockstep with the authoritative
// PodSandbox State so the two protos can never report different states. The
// PodSandbox proto wins: a caller that mutates only the status State will
// have that change discarded at the next Add/Update.
func (s *Sandbox) reconcile() {
	if s != nil && s.p != nil && s.s != nil {
		s.s.State = s.p.State
	}
}

func (c *Container) Container() *criv1.Container {
	if c == nil {
		return nil
	}
	return c.p
}

func (c *Container) ContainerStatus() *criv1.ContainerStatus {
	if c == nil {
		return nil
	}
	return c.s
}

// Usage returns the container's accumulated resource usage.
func (c *Container) Usage() Usage {
	if c == nil {
		return Usage{}
	}
	return c.usage
}

// state returns the authoritative container state: the Container State when
// present, falling back to the status. The two are kept equal by reconcile.
func (c *Container) state() criv1.ContainerState {
	if c != nil && c.p != nil {
		return c.p.State
	}
	if c != nil && c.s != nil {
		return c.s.State
	}
	return criv1.ContainerState_CONTAINER_CREATED
}

// reconcile keeps the status State in lockstep with the authoritative
// Container State so the two protos can never report different states.
func (c *Container) reconcile() {
	if c != nil && c.p != nil && c.s != nil {
		c.s.State = c.p.State
	}
}

func (img *Image) ImageSpec() *criv1.ImageSpec {
	if img == nil {
		return nil
	}
	return img.i
}

func (s *Sandbox) Clone() *Sandbox {
	if s == nil {
		return nil
	}
	var p *criv1.PodSandbox
	if s.p != nil {
		p = proto.Clone(s.p).(*criv1.PodSandbox)
	}
	var status *criv1.PodSandboxStatus
	if s.s != nil {
		status = proto.Clone(s.s).(*criv1.PodSandboxStatus)
	}
	return &Sandbox{p: p, s: status, usage: s.usage}
}

func (c *Container) Clone() *Container {
	if c == nil {
		return nil
	}
	var p *criv1.Container
	if c.p != nil {
		p = proto.Clone(c.p).(*criv1.Container)
	}
	var status *criv1.ContainerStatus
	if c.s != nil {
		status = proto.Clone(c.s).(*criv1.ContainerStatus)
	}
	return &Container{p: p, s: status, spec: c.spec, usage: c.usage}
}

func (img *Image) Clone() *Image {
	if img == nil {
		return nil
	}
	var i *criv1.ImageSpec
	if img.i != nil {
		i = proto.Clone(img.i).(*criv1.ImageSpec)
	}
	return &Image{i: i}
}

// Matching Functions
func (x *Sandbox) Match(f *criv1.PodSandboxFilter) bool {
	if f == nil {
		return true
	}

	if f.Id != "" {
		if x.p == nil || x.p.Id != f.Id {
			return false
		}
	}

	if f.State != nil {
		if x.state() != f.State.State {
			return false
		}
	}

	if f.GetLabelSelector() != nil && x.p != nil {
		if !labels.ValidatedSetSelector(f.GetLabelSelector()).Matches(labels.Set(x.p.GetLabels())) {
			return false
		}
	}

	return true
}

func (x *Container) Match(f *criv1.ContainerFilter) bool {
	if f == nil {
		return true
	}

	if f.Id != "" {
		if x.p == nil || x.p.Id != f.Id {
			return false
		}
	}

	if f.PodSandboxId != "" {
		if x.p == nil || x.p.PodSandboxId != f.PodSandboxId {
			return false
		}
	}

	if f.State != nil {
		if x.state() != f.State.State {
			return false
		}
	}

	if f.GetLabelSelector() != nil && x.p != nil {
		if !labels.ValidatedSetSelector(f.GetLabelSelector()).Matches(labels.Set(x.p.GetLabels())) {
			return false
		}
	}

	return true
}

func (x *Image) Match(f *criv1.ImageFilter) bool {
	if f == nil {
		return true
	}

	if f.Image != nil && x.i != nil {
		if x.i.Image != f.Image.Image {
			return false
		}
	}

	return true
}

// Runtime API implementation
func NewRuntime(tickRate time.Duration) *Runtime {
	return New(Options{TickRate: tickRate})
}

// Enqueue schedules callback after delayTicks ticks and blocks until it has
// run. Deprecated: use Schedule for asynchronous callbacks, or Submit with
// Future.Wait for atomic commits.
func (r *Runtime) Enqueue(delayTicks uint64, callback func(r *Runtime)) <-chan struct{} {
	f := r.Schedule(Ticks(delayTicks), callback)
	<-f.Done()
	return nil
}

func (r *Runtime) AddSandbox(id string, sb *Sandbox) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := sb.Clone()
	c.reconcile()
	r.sandboxes[id] = c
	r.emit(Event{Kind: EventSandboxCreated, ID: id, ToSB: c.state(), At: r.now})
}

func (r *Runtime) RemoveSandbox(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sandboxes[id]; !ok {
		return
	}
	delete(r.sandboxes, id)
	r.emit(Event{Kind: EventSandboxRemoved, ID: id, At: r.now})
}

func (r *Runtime) UpdateSandbox(id string, sb *Sandbox) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.sandboxes[id]
	c := sb.Clone()
	c.reconcile()
	r.sandboxes[id] = c
	if old != nil && old.state() != c.state() {
		r.emit(Event{Kind: EventSandboxStateChanged, ID: id, FromSB: old.state(), ToSB: c.state(), At: r.now})
	}
}

func (r *Runtime) ListSandboxes(f *criv1.PodSandboxFilter) []*Sandbox {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []*Sandbox
	for _, sb := range r.sandboxes {
		if sb.Match(f) {
			list = append(list, sb.Clone())
		}
	}
	return list
}

func (r *Runtime) GetSandbox(id string) (*Sandbox, bool) {
	list := r.ListSandboxes(&criv1.PodSandboxFilter{Id: id})
	if len(list) == 0 {
		return nil, false
	}
	return list[0], true
}

// Container Management
func (r *Runtime) AddContainer(id string, c *Container) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cc := c.Clone()
	cc.reconcile()
	r.containers[id] = cc
	if cc.p != nil {
		r.emit(Event{Kind: EventContainerCreated, ID: id, Sandbox: cc.p.PodSandboxId, ToC: cc.state(), At: r.now})
	}
}

func (r *Runtime) RemoveContainer(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.containers[id]; !ok {
		return
	}
	delete(r.containers, id)
	r.emit(Event{Kind: EventContainerRemoved, ID: id, At: r.now})
}

func (r *Runtime) UpdateContainer(id string, c *Container) {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.containers[id]
	cc := c.Clone()
	cc.reconcile()
	r.containers[id] = cc
	if old != nil && old.state() != cc.state() && cc.p != nil {
		r.emit(Event{Kind: EventContainerStateChanged, ID: id, Sandbox: cc.p.PodSandboxId,
			FromC: old.state(), ToC: cc.state(), At: r.now})
	}
}

func (r *Runtime) ListContainers(f *criv1.ContainerFilter) []*Container {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []*Container
	for _, c := range r.containers {
		if c.Match(f) {
			list = append(list, c.Clone())
		}
	}
	return list
}

func (r *Runtime) GetContainer(id string) (*Container, bool) {
	list := r.ListContainers(&criv1.ContainerFilter{Id: id})
	if len(list) == 0 {
		return nil, false
	}
	return list[0], true
}

// Image Management
func (r *Runtime) AddImage(id string, img *Image) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images[id] = img.Clone()
	r.emit(Event{Kind: EventImagePulled, ID: id, At: r.now})
}

func (r *Runtime) RemoveImage(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.images[id]; !ok {
		return
	}
	delete(r.images, id)
	r.emit(Event{Kind: EventImageRemoved, ID: id, At: r.now})
}

func (r *Runtime) UpdateImage(id string, img *Image) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images[id] = img.Clone()
}

func (r *Runtime) ListImages(f *criv1.ImageFilter) []*Image {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []*Image
	for _, img := range r.images {
		if img.Match(f) {
			list = append(list, img.Clone())
		}
	}
	return list
}

func (r *Runtime) GetImage(id string) (*Image, bool) {
	var imgSpec *criv1.ImageSpec
	if id != "" {
		imgSpec = &criv1.ImageSpec{Image: id}
	}
	list := r.ListImages(&criv1.ImageFilter{Image: imgSpec})
	if len(list) == 0 {
		return nil, false
	}
	return list[0], true
}

func (r *Runtime) ListPodSandboxStats(f *criv1.PodSandboxStatsFilter) []*criv1.PodSandboxStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []*criv1.PodSandboxStats
	for _, sb := range r.sandboxes {
		if sb.p == nil {
			continue
		}
		if f == nil || f.Id == "" || sb.p.Id == f.Id {
			list = append(list, r.materializePodSandboxStats(sb))
		}
	}
	return list
}

func (r *Runtime) GetPodSandboxStats(id string) (*criv1.PodSandboxStats, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sb, ok := r.sandboxes[id]
	if !ok {
		return nil, false
	}
	return r.materializePodSandboxStats(sb), true
}

func (r *Runtime) ListContainerStats(f *criv1.ContainerStatsFilter) []*criv1.ContainerStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	var cf *criv1.ContainerFilter
	if f != nil {
		cf = &criv1.ContainerFilter{
			Id:            f.Id,
			PodSandboxId:  f.PodSandboxId,
			LabelSelector: f.LabelSelector,
		}
	}
	var list []*criv1.ContainerStats
	for _, c := range r.containers {
		if c.Match(cf) {
			list = append(list, r.materializeContainerStats(c))
		}
	}
	return list
}

func (r *Runtime) GetContainerStats(id string) (*criv1.ContainerStats, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.containers[id]
	if !ok {
		return nil, false
	}
	return r.materializeContainerStats(c), true
}
