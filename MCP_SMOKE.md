context7 library ID: /kubernetes-sigs/controller-runtime
retrieved docs (first ~300 chars):
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
