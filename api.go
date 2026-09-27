package runkube

import (
	"github.com/hoskeri/runkube/pkg/kapi"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var defaultAPIGroups = []*kapi.Group{
	{
		GroupVersion: schema.GroupVersion{Group: "api.runkube.internal", Version: "v1"},
		Resources: []*kapi.Resource{
			{
				GVK:      schema.GroupVersionKind{Group: "api.runkube.internal", Version: "v1", Kind: "Bucket"},
				Resource: "buckets",
			},
		},
	},
	{
		GroupVersion: schema.GroupVersion{Group: "example-b.runkube.internal", Version: "v1"},
		Resources: []*kapi.Resource{
			{
				GVK:        schema.GroupVersionKind{Group: "example-b.runkube.internal", Version: "v1", Kind: "ExampleB"},
				Namespaced: true,
				Resource:   "examplesb",
			},
		},
	},
}
