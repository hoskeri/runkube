package model

import (
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/labels"
	criv1 "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// Event represents a scheduled simulation task.
type Event struct {
	Sequence uint64
	Callback func(r *Runtime)
}

// Runtime tracks the objects in container runtime discrete event model.
type Runtime struct {
	mu         sync.Mutex
	sandboxes  map[string]*Sandbox
	containers map[string]*Container
	images     map[string]*Image

	// Simulation queue properties
	tickRate time.Duration
	seq      uint64
	events   []Event
}

// Sandbox model
type Sandbox struct {
	p     *criv1.PodSandbox
	s     *criv1.PodSandboxStatus
	stats *criv1.PodSandboxStats
}

// Container model
type Container struct {
	p     *criv1.Container
	s     *criv1.ContainerStatus
	stats *criv1.ContainerStats
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

func (s *Sandbox) PodSandboxStats() *criv1.PodSandboxStats {
	if s == nil {
		return nil
	}
	return s.stats
}

func (s *Sandbox) SetPodSandboxStats(stats *criv1.PodSandboxStats) {
	if s != nil {
		s.stats = stats
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

func (c *Container) ContainerStats() *criv1.ContainerStats {
	if c == nil {
		return nil
	}
	return c.stats
}

func (c *Container) SetContainerStats(stats *criv1.ContainerStats) {
	if c != nil {
		c.stats = stats
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
	var stats *criv1.PodSandboxStats
	if s.stats != nil {
		stats = proto.Clone(s.stats).(*criv1.PodSandboxStats)
	}
	return &Sandbox{p: p, s: status, stats: stats}
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
	var stats *criv1.ContainerStats
	if c.stats != nil {
		stats = proto.Clone(c.stats).(*criv1.ContainerStats)
	}
	return &Container{p: p, s: status, stats: stats}
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
		if x.s == nil || x.s.State != f.State.State {
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
		if x.s == nil || x.s.State != f.State.State {
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
	r := &Runtime{
		sandboxes:  make(map[string]*Sandbox),
		containers: make(map[string]*Container),
		images:     make(map[string]*Image),
		tickRate:   tickRate,
	}
	if tickRate > 0 {
		go r.runSimulation()
	}
	return r
}

// Enqueue registers a delayed callback and returns a channel that closes when the event completes.
func (r *Runtime) Enqueue(delayTicks uint64, callback func(r *Runtime)) <-chan struct{} {
	r.mu.Lock()
	targetSeq := r.seq + delayTicks
	done := make(chan struct{})
	r.events = append(r.events, Event{
		Sequence: targetSeq,
		Callback: func(rt *Runtime) {
			callback(rt)
			close(done)
		},
	})

	r.mu.Unlock()
	<-done
	return nil
}

// runSimulation runs the background ticker.
func (r *Runtime) runSimulation() {
	ticker := time.NewTicker(r.tickRate)
	defer ticker.Stop()
	for range ticker.C {
		r.mu.Lock()
		r.seq++
		currentSeq := r.seq

		var toRun []Event
		var remaining []Event
		for _, ev := range r.events {
			if ev.Sequence <= currentSeq {
				toRun = append(toRun, ev)
			} else {
				remaining = append(remaining, ev)
			}
		}
		r.events = remaining
		r.mu.Unlock()

		for _, ev := range toRun {
			ev.Callback(r)
		}
	}
}

func (r *Runtime) AddSandbox(id string, sb *Sandbox) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandboxes[id] = sb.Clone()
}

func (r *Runtime) RemoveSandbox(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sandboxes, id)
}

func (r *Runtime) UpdateSandbox(id string, sb *Sandbox) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sandboxes[id] = sb.Clone()
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
	r.containers[id] = c.Clone()
}

func (r *Runtime) RemoveContainer(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.containers, id)
}

func (r *Runtime) UpdateContainer(id string, c *Container) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.containers[id] = c.Clone()
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
}

func (r *Runtime) RemoveImage(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.images, id)
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
		if sb.stats != nil {
			if f == nil || f.Id == "" || sb.p.Id == f.Id {
				list = append(list, proto.Clone(sb.stats).(*criv1.PodSandboxStats))
			}
		}
	}
	return list
}

func (r *Runtime) GetPodSandboxStats(id string) (*criv1.PodSandboxStats, bool) {
	sb, ok := r.GetSandbox(id)
	if !ok || sb.stats == nil {
		return nil, false
	}
	return proto.Clone(sb.stats).(*criv1.PodSandboxStats), true
}

func (r *Runtime) ListContainerStats(f *criv1.ContainerStatsFilter) []*criv1.ContainerStats {
	cf := &criv1.ContainerFilter{
		Id:            f.Id,
		PodSandboxId:  f.PodSandboxId,
		LabelSelector: f.LabelSelector,
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var list []*criv1.ContainerStats
	for _, c := range r.containers {
		if c.stats != nil {
			if c.Match(cf) {
				list = append(list, proto.Clone(c.stats).(*criv1.ContainerStats))
			}
		}
	}
	return list
}

func (r *Runtime) GetContainerStats(id string) (*criv1.ContainerStats, bool) {
	c, ok := r.GetContainer(id)
	if !ok || c.stats == nil {
		return nil, false
	}
	return proto.Clone(c.stats).(*criv1.ContainerStats), true
}
