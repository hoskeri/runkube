package main

import (
	"context"
	"iter"
	"log/slog"
	"os"
	"os/signal"
	goruntime "runtime"
	"strings"
	"syscall"

	"github.com/spf13/pflag"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

type requestInfo struct {
	Group         string
	Version       string
	Resource      string
	Subresource   string
	Verb          string
	FieldSelector string
	LabelSelector string
	Namespace     string
	Name          string
}

func (r *requestInfo) ToRESTRequest(c *rest.RESTClient) *rest.Request {
	req := rest.NewRequest(c)

	switch r.Group {
	case "":
		req = req.Prefix("api", r.Version)
	default:
		req = req.Prefix("apis", r.Group, r.Version)
	}

	switch v := strings.ToLower(r.Verb); v {
	case "create":
		req.Verb("POST")
	case "update", "patch":
		req.Verb("PUT")
	case "delete", "deletecollection":
		req.Verb("DELETE")
	case "get", "list", "watch":
		req.Verb("GET")
	default:
		panic("unknown verb: " + v + ".")
	}

	req = req.MaxRetries(0)
	req = req.Namespace(r.Namespace)
	req = req.Resource(r.Resource)
	req = req.SubResource(r.Subresource)

	if r.FieldSelector != "" {
		req.Param("fieldSelector", r.FieldSelector)
	}

	if r.LabelSelector != "" {
		req.Param("labelSelector", r.LabelSelector)
	}

	return req
}

func DiscoveryExpandor(base requestInfo, c discovery.DiscoveryInterface) (iter.Seq[requestInfo], error) {
	_, apiGroups, err := c.ServerGroupsAndResources()
	if err != nil {
		return nil, err
	}

	return func(yield func(r requestInfo) bool) {
		for _, g := range apiGroups {
			for _, a := range g.APIResources {
				gv, _ := schema.ParseGroupVersion(g.GroupVersion)
				for _, verb := range a.Verbs {
					r := base
					r.Verb = verb
					r.Version = gv.Version
					r.Group = gv.Group
					r1, r2, _ := strings.Cut(a.Name, "/")
					r.Resource = r1
					r.Subresource = r2

					if !yield(r) {
						return
					}
				}
			}
		}
	}, nil
}

type expandor struct {
	template requestInfo

	discovery iter.Seq[requestInfo]
	namespace iter.Seq[requestInfo]
}

func (e *expandor) All() iter.Seq[requestInfo] {
	return func(yield func(r requestInfo) bool) {
		for {
			if e.discovery == nil {
				return
			}
			for disco := range e.discovery {
				if !yield(disco) {
					return
				}
			}
		}
	}
}

type Options struct {
	Concurrency int  `json:"concurrency,omitempty"`
	Debug       bool `json:"debug,omitempty"`
}

func (o *Options) AddFlags(fs *pflag.FlagSet) {
	fs.IntVar(&o.Concurrency, "concurrency", goruntime.NumCPU(), "request concurrency limit, defaults to goruntime.NumCPU()")
	fs.BoolVar(&o.Debug, "debug", false, "debug logging")
}

func init() {
	utilruntime.ErrorHandlers = []utilruntime.ErrorHandler{}
}

func main() {
	kubeConfigFlags := genericclioptions.NewConfigFlags(false)
	kubeConfigFlags.AddFlags(pflag.CommandLine)

	requestorFlags := &Options{}
	requestorFlags.AddFlags(pflag.CommandLine)

	pflag.Parse()

	logLevel := slog.LevelInfo
	if requestorFlags.Debug {
		logLevel = slog.LevelDebug
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: false,
		Level:     logLevel,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey, slog.LevelKey:
				return slog.Attr{}
			}
			return a
		},
	})))

	fatally := func(tag string, err error) {
		if err != nil {
			slog.Error("exiting, fatal error in "+tag, "err", err)
			os.Exit(1)
		}
	}

	kubeConfigFlags.WrapConfigFn = func(in *rest.Config) *rest.Config {
		restConfig := rest.CopyConfig(in)
		restConfig.UserAgent = "requestor/v1.0.0"
		restConfig.DisableCompression = true
		restConfig.QPS = -1
		restConfig.GroupVersion = &schema.GroupVersion{}
		restConfig.WarningHandler = rest.NoWarnings{}
		restConfig.NegotiatedSerializer = runtime.NewSimpleNegotiatedSerializer(runtime.SerializerInfo{})
		return restConfig
	}

	restConfig, err := kubeConfigFlags.ToRESTConfig()
	fatally("flags.ToRESTConfig", err)

	dc, err := kubeConfigFlags.ToDiscoveryClient()
	fatally("kcf.ToDiscoveryClient", err)

	de, err := DiscoveryExpandor(requestInfo{}, dc)
	fatally("DiscoveryExpander", err)

	exp := &expandor{
		template:  requestInfo{},
		discovery: de,
	}

	c, err := rest.RESTClientFor(restConfig)
	fatally("rest.UnversionedRESTClientFor", err)

	ctx := context.Background()
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	go func() {
		requests, ctx := errgroup.WithContext(ctx)
		requests.SetLimit(requestorFlags.Concurrency)

		for o := range exp.All() {
			r := o.ToRESTRequest(c)
			if err := r.Error(); err != nil {
				slog.Info("skipped", "err", err)
				continue
			}

			requests.Go(func() error {
				result := r.Do(ctx)
				res, err := result.Raw()
				slog.Debug("o", "uri", r.URL(), "res", len(res), "err", err)
				return err
			})
		}

		slog.Debug("request.Wait")
		err := requests.Wait()
		slog.Info("done waiting", "err", err)
	}()

	slog.Debug("ctx.Done")
	<-ctx.Done()
}
