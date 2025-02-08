package syncer

import (
	"bytes"
	"context"

	"go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	discovery "k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

type Syncer struct {
	objects []*unstructured.Unstructured
}

type Client struct {
	client dynamic.Interface
	mapper meta.RESTMapper
	dsc    discovery.CachedDiscoveryInterface
}

func NewSyncer() (*Syncer, error) {
	return &Syncer{}, nil
}

func NewClient(ri rest.Interface) (*Client, error) {
	dc := dynamic.New(ri)
	mc := memory.NewMemCacheClient(discovery.NewDiscoveryClient(ri))
	c := &Client{
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(mc),
		client: dc,
		dsc:    mc,
	}

	return c, nil
}

func (s *Syncer) Add(objs ...runtime.Object) error {
	for _, o := range objs {
		u := &unstructured.Unstructured{}
		c, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
		if err != nil {
			return err
		}

		u.SetUnstructuredContent(c)
		s.objects = append(s.objects, u)
	}
	return nil
}

func (s *Syncer) AsYAML() []byte {
	w := &bytes.Buffer{}
	ye := yaml.NewEncoder(w)
	ye.CompactSeqIndent()
	for _, o := range s.objects {
		uns := o.UnstructuredContent()
		if err := ye.Encode(uns); err != nil {
			panic(err)
		}
	}

	return w.Bytes()
}

func (s *Client) SyncOne(ctx context.Context, o *unstructured.Unstructured) error {
	m, err := s.mapper.RESTMapping(o.GroupVersionKind().GroupKind(), o.GroupVersionKind().Version)
	if err != nil {
		return err
	}
	_, err = s.client.Resource(m.Resource).Create(ctx, o, metav1.CreateOptions{})
	return err
}
