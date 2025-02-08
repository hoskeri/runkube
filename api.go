package runkube

import (
	"github.com/hoskeri/runkube/pkg/kapi"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var defaultAPIGroups = []*kapi.Group{
	{
		APIVersion: schema.GroupVersion{Group: "api.runkube.internal", Version: "v1"},
		Resources: []kapi.Resource{
			&kapi.ResourceSpec{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "api.runkube.internal",
					Version: "v1",
					Kind:    "Bucket",
				},
				Resource: "buckets",
			},
		},
	},
	{
		APIVersion: schema.GroupVersion{Group: "example-b.runkube.internal", Version: "v1"},
		Resources: []kapi.Resource{
			&kapi.ResourceSpec{
				GroupVersionKind: schema.GroupVersionKind{
					Group:   "example-b.runkube.internal",
					Version: "v1",
					Kind:    "ExampleB",
				},
				Namespaced: true,
				Resource:   "examplesb",
			},
		},
	},
}
