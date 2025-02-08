package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
)

func uidKeyFunc(obj any) (string, error) {
	a, err := meta.Accessor(obj)
	if err != nil {
		return "", err
	}

	return string(a.GetUID()), nil
}

func dimensions(gr string, i any) (map[string]string, error) {
	o := i.(*metav1.PartialObjectMetadata)
	ma, err := meta.Accessor(o)
	if err != nil {
		return nil, err
	}

	d := map[string]string{
		"gr": gr,
	}

	l := ma.GetLabels()
	r, ok := l["runkube.internal/realm"]
	if ok {
		d["realm"] = r
		d["realm.gr"] = r + "+" + gr
	}

	return d, nil
}

func makeIndexFunc(k string) cache.IndexFunc {
	ki := k
	return func(o any) ([]string, error) {
		ma, err := meta.Accessor(o)
		if err != nil {
			return nil, err
		}

		v, ok := ma.GetLabels()[ki]
		if !ok {
			return nil, nil
		}
		return []string{v}, nil
	}
}

type Quotable struct {
	client          metadata.Interface
	dc              discovery.CachedDiscoveryInterface
	informers       metadatainformer.SharedInformerFactory
	quotableIndexer cache.Indexer

	apiHandler *http.ServeMux
}

func (q *Quotable) newIndexer() cache.Indexer {
	return cache.NewIndexer(uidKeyFunc, cache.Indexers{
		"gr":       makeIndexFunc("gr"),
		"realm.gr": makeIndexFunc("realm.gr"),
	})
}

func (q *Quotable) OnAdd(i any, initial bool) {
	if err := q.quotableIndexer.Add(i); err != nil {
		slog.Error("add.error", "err", err)
	}
}

func (q *Quotable) OnDelete(i any) {
	q.quotableIndexer.Delete(i)
	if err := q.quotableIndexer.Add(i); err != nil {
		slog.Error("delete.error", "err", err)
	}
}

func (q *Quotable) Count(k, v string) (int, error) {
	x, err := q.quotableIndexer.IndexKeys(k, v)
	if err != nil {
		return math.MaxInt, err
	}

	return len(x), nil
}

func (q *Quotable) Dimensions() []string {
	return slices.Collect(maps.Keys(q.quotableIndexer.GetIndexers()))
}

func (q *Quotable) Values(dim string) []string {
	return q.quotableIndexer.ListIndexFuncValues(dim)
}

// Satisfy cache.ResourceEventHandler, we are not really interested in update events.
func (q *Quotable) OnUpdate(_, _ any) {}

func (q *Quotable) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	q.apiHandler.ServeHTTP(w, req)
}

func (q *Quotable) setupAPI() {
	q.apiHandler = http.NewServeMux()
	q.apiHandler.HandleFunc("GET /dimensions", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(q.Dimensions())
	})

	q.apiHandler.HandleFunc("GET /dimensions/{dim}/values", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(q.Values(req.PathValue("dim")))
	})

	q.apiHandler.HandleFunc("GET /dimensions/{dim}/values/{value}", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("content-type", "application/json")
		c, err := q.Count(req.PathValue("dim"), req.PathValue("value"))
		if err != nil {
			slog.Error("get count", "err", err)
			fmt.Fprintf(w, "error: %s", err.Error())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		v := struct {
			Dimension string
			Value     string
			Count     int
		}{
			Dimension: req.PathValue("dim"),
			Value:     req.PathValue("value"),
			Count:     c,
		}
		json.NewEncoder(w).Encode(v)
	})
}

func quotableRestConfig(in *rest.Config) *rest.Config {
	restConfig := rest.CopyConfig(in)
	restConfig.UserAgent = "quotable/v1.0.0"
	restConfig.QPS = -1
	restConfig.GroupVersion = &schema.GroupVersion{}
	restConfig.WarningHandler = rest.NoWarnings{}
	restConfig.NegotiatedSerializer = runtime.NewSimpleNegotiatedSerializer(runtime.SerializerInfo{})
	return restConfig
}

func New(ic *rest.Config) (*Quotable, error) {
	c := quotableRestConfig(ic)

	var err error
	q := &Quotable{}
	q.quotableIndexer = q.newIndexer()

	q.client, err = metadata.NewForConfig(c)
	q.setupAPI()
	if err != nil {
		return nil, err
	}

	adc, err := discovery.NewDiscoveryClientForConfig(c)
	if err != nil {
		return nil, err
	}
	q.dc = memory.NewMemCacheClient(adc)
	q.informers = metadatainformer.NewSharedInformerFactoryWithOptions(q.client, 0)
	return q, nil
}

func gvrKey(a schema.GroupVersionResource) string {
	if a.Group == "" {
		a.Group = "core"
	}
	return strings.Join([]string{a.Resource, a.Version, a.Group}, ".")
}

func (q *Quotable) run(ctx context.Context) error {
	rs, err := q.getResources()
	if err != nil {
		return err
	}

	for _, a := range rs.UnsortedList() {
		slog.Debug("rs", "empty", a.Empty(), "g", a.Group, "v", a.Version, "r", a.Resource)
		q.informers.ForResource(a).Informer().SetTransform(q.transformFunc(gvrKey(a)))
		_, err := q.informers.ForResource(a).Informer().AddEventHandler(q)
		if err != nil {
			return err
		}
	}
	q.informers.Start(ctx.Done())
	<-ctx.Done()
	return nil
}

func (q *Quotable) transformFunc(gr string) cache.TransformFunc {
	return func(i any) (any, error) {
		m, ok := i.(*metav1.PartialObjectMetadata)
		if !ok {
			return nil, fmt.Errorf("transform.Cast %T", i)
		}

		m.SetAnnotations(nil)
		m.SetManagedFields(nil)
		m.SetOwnerReferences(nil)
		m.SetFinalizers(nil)
		m.SetResourceVersion("")

		// We take over object labels store our dimensions.
		// object's original labels are discarded.
		dims, err := dimensions(gr, i)
		if err != nil {
			return nil, err
		}

		m.SetLabels(dims)
		return i, nil
	}
}

var (
	storageVerbs = []string{
		"create",
		"update",
		"delete",
	}
)

func (q *Quotable) getResources() (sets.Set[schema.GroupVersionResource], error) {
	_, agr, err := q.dc.ServerGroupsAndResources()
	if err != nil {
		return nil, err
	}

	rs := sets.New[schema.GroupVersionResource]()
	for _, g := range agr {
		gv, err := schema.ParseGroupVersion(g.GroupVersion)
		if err != nil {
			return nil, err
		}

		for _, a := range g.APIResources {
			// Don't care about subresources.
			if strings.Contains(a.Name, "/") {
				continue
			}

			// If a resource is not stored, we are not interested in it.
			if !sets.New(a.Verbs...).HasAll(storageVerbs...) {
				slog.Debug("getResources", "skipping", a)
				continue
			}

			rs.Insert(gv.WithResource(a.Name))
		}
	}
	return rs, nil
}

func main() {
	ctx := context.Background()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	kubeConfigFlags := genericclioptions.NewConfigFlags(false)
	kubeConfigFlags.AddFlags(pflag.CommandLine)
	pflag.Parse()

	logLevel := slog.LevelDebug
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: false,
		Level:     logLevel,
	})))

	fatally := func(tag string, err error) {
		if err != nil {
			slog.Error("exiting, fatal error in "+tag, "err", err)
			os.Exit(1)
		}
	}

	restConfig, err := kubeConfigFlags.ToRESTConfig()
	fatally("flags.ToRESTConfig", err)

	q, err := New(restConfig)
	fatally("quotable.New", err)

	slog.Info("starting")
	eg, ctx := errgroup.WithContext(ctx)
	s := &http.Server{
		Addr:                         ":8081",
		Handler:                      q,
		DisableGeneralOptionsHandler: true,
	}

	eg.Go(func() error {
		err := s.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("s.ListenAndServe: %w", err)
		}
		return nil
	})
	eg.Go(func() error { <-ctx.Done(); return s.Shutdown(ctx) })
	eg.Go(func() error { return q.run(ctx) })
	fatally("run", eg.Wait())
}
