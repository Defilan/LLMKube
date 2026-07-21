context7 library ID: /kubernetes-sigs/controller-runtime
retrieved docs (first ~300 chars):
### FieldIndexer Interface Definition

Source: https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/client/interfaces.go

Defines the IndexField method interface that allows indexing Kubernetes objects by custom fields for efficient field-based queries. The method takes a context, object, field name, and an IndexerFunc to extract values.

```go
// FieldIndexer knows how to index over a particular "field" such that it
// can later be used by a field selector.
type FieldIndexer interface {
	// IndexField adds an index with the given field name on the given object type
	// by using the given function to extract the value for that field.  If you want
	// compatibility with the Kubernetes API server, only return one key, and only use
	// fields that the API server supports.  Otherwise, you can return multiple keys,
	// and "equality" in the field selector means that at least one key matches the value.
	// The FieldIndexer will automatically take care of indexing over namespace
	// and supporting efficient all-namespace queries.
	IndexField(ctx context.Context, obj Object, field string, extractValue IndexerFunc) error
}
```

--------------------------------

### IndexerFunc Type Definition

Source: https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/client/interfaces.go

Defines the function signature for extracting index keys from objects. It takes a client.Object and returns a slice of strings representing the index values.

```go
// IndexerFunc knows how to take an object and turn it into a series
// of non-namespaced keys. Namespaced objects are automatically given
// namespaced and non-spaced variants, so keys do not need to include namespace.
type IndexerFunc func(Object) []string
```

--------------------------------

### IndexField Implementation in informerCache

Source: https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/cache/informer_cache.go

The concrete implementation of IndexField in the informer cache that gets an informer for the object and registers an index using the provided field name and value extraction function.

```go
// IndexField adds an indexer to the underlying informer, using extractValue function to get
// value(s) from the given field. This index can then be used by passing a field selector
// to List. For one-to-one compatibility with "normal" field selectors, only return one value.
// The values may be anything. They will automatically be prefixed with the namespace of the
// given object, if present. The objects passed are guaranteed to be objects of the correct type.
func (ic *informerCache) IndexField(ctx context.Context, obj client.Object, field string, extractValue client.IndexerFunc) error {
	informer, err := ic.GetInformer(ctx, obj, BlockUntilSynced(false))
	if err != nil {
		return err
	}
	return indexByField(informer, field, extractValue)
}
```

--------------------------------

### New Cluster Interface Definition

Source: https://github.com/kubernetes-sigs/controller-runtime/blob/main/designs/move-cluster-specific-code-out-of-manager.md

Defines the `Cluster` interface, encapsulating all cluster-specific operations such as configuration retrieval, client access, cache management, and event recording.

```go
type Cluster interface {
	// SetFields will set cluster-specific dependencies on an object for which the object has implemented the inject
	// interface, specifically inject.Client, inject.Cache, inject.Scheme, inject.Config and inject.APIReader
	SetFields(interface{}) error

	// GetConfig returns an initialized Config
	GetConfig() *rest.Config

	// GetClient returns a client configured with the Config. This client may
	// not be a fully "direct" client -- it may read from a cache,
	// for instance.  See Options.NewClient for more information on how the default
	// implementation works.
	GetClient() client.Client

	// GetFieldIndexer returns a client.FieldIndexer configured with the client
	GetFieldIndexer() client.FieldIndexer

	// GetCache returns a cache.Cache
	GetCache() cache.Cache

	// GetEventRecorderFor returns a new EventRecorder for the provided name
	GetEventRecorderFor(name string) record.EventRecorder

	// GetRESTMapper returns a RESTMapper
	GetRESTMapper() meta.RESTMapper

	// GetAPIReader returns a reader that will be configured to use the API server.
	// This should be used sparingly and only when the client does not fit your
	// use case.
	GetAPIReader() client.Reader

	// GetScheme returns an initialized Scheme
	GetScheme() *runtime.Scheme

	// Start starts the connection tothe Cluster
	Start(<-chan struct{}) error
}
```

--------------------------------

### Configure Filtered Cache with Selectors

Source: https://github.com/kubernetes-sigs/controller-runtime/blob/main/designs/use-selectors-at-cache.md

Example of overriding the default NewCache function to use a filtered cache. This configuration specifies selectors for corev1.Node and v1beta1.NodeNetworkState objects.

```go
ctrl.Options.NewCache = cache.BuilderWithOptions(cache.Options{
                            SelectorsByObject: cache.SelectorsByObject{
                                    &corev1.Node{}: {
                                        Field: fields.SelectorFromSet(fields.Set{"metadata.name": "node01"}),
                                    }
                                    &v1beta1.NodeNetworkState{}: {
                                        Field: fields.SelectorFromSet(fields.Set{"metadata.name": "node01"}),
                                        Label: labels.SelectorFromSet(labels.Set{"app": "kubernetes-nmstate})",
                                    }
                                }
                            }
                        )

```

