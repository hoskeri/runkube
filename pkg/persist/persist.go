package persist

import (
	"bytes"
	"errors"
	"strconv"
	"sync"

	bolt "go.etcd.io/bbolt"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/api/validation/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
)

var (
	ErrDoesNotExist  = errors.New("referenced key does not exist")
	ErrAlreadyExists = errors.New("referenced key already exists")
)

type Reference struct {
	key []byte
	raw [][]byte
}

var clusterScope = []byte("SCOPE_CLUSTER")
var namespaceScope = []byte("SCOPE_NAMESPACE")
var defaultRealm = []byte("REALM_DEFAULT")

func (r *Reference) restScope() meta.RESTScope {
	if bytes.Equal(r.scope(), clusterScope) {
		return meta.RESTScopeRoot
	}

	if bytes.Equal(r.scope(), namespaceScope) {
		return meta.RESTScopeNamespace
	}

	return nil
}

func (r *Reference) realm() []byte {
	return r.raw[0]
}

func (r *Reference) scope() []byte {
	return r.raw[1]
}

func (r *Reference) group() []byte {
	return r.raw[2]
}

func (r *Reference) version() []byte {
	return r.raw[3]
}

func (r *Reference) resource() []byte {
	return r.raw[4]
}

func (r *Reference) namespace() []byte {
	return r.raw[5]
}

func (r *Reference) name() []byte {
	return r.raw[6]
}

var (
	ErrParseRef              = errors.New("error parsing reference")
	ErrObjectsBucketNotFound = errors.New("objects bucket not found")
)

func fromStorage(b []byte, _ []byte) (*Reference, error) {
	r := &Reference{
		key: b,
	}
	c := bytes.Split(b, refSeparator)
	if len(c) != 8 {
		return nil, ErrParseRef
	}

	r.raw = c[0:7]

	return r, nil
}

func fromObject(rm *meta.RESTMapping, mo metav1.Object) (*Reference, error) {
	r := &Reference{}
	r.raw = make([][]byte, 7, 7)

	r.raw[0] = []byte(defaultRealm)

	switch rm.Scope {
	case meta.RESTScopeRoot:
		r.raw[1] = clusterScope
	case meta.RESTScopeNamespace:
		r.raw[1] = namespaceScope
		r.raw[5] = []byte(mo.GetNamespace())
	default:
		return nil, ErrParseRef
	}

	r.raw[2] = []byte(rm.Resource.Group)
	r.raw[3] = []byte(rm.Resource.Version)
	// r.raw[5] namespace set above.
	r.raw[4] = []byte(rm.Resource.Resource)
	r.raw[6] = []byte(mo.GetName())

	k := bytes.NewBuffer(make([]byte, 0, 255))
	for _, i := range r.raw {
		k.Write(i)
		k.Write(refSeparator)
	}

	r.key = k.Bytes()
	return r, nil
}

type Matcher struct {
	realm     string
	rm        *meta.RESTMapping
	namespace string
	name      string
}

func (m *Matcher) Matches(r *Reference) int {
	if !bytes.Equal([]byte(m.realm), r.realm()) {
		return 1
	}

	if m.rm.Resource.Group != string(r.group()) {
		return 2
	}

	if m.rm.Resource.Version != string(r.version()) {
		return 3
	}

	if m.rm.Resource.Resource != string(r.resource()) {
		return 4
	}

	s := r.restScope()
	if m.rm.Scope != s {
		return 5
	}

	switch s {
	case meta.RESTScopeRoot:
		if len(r.namespace()) > 0 {
			return 6
		}
	case meta.RESTScopeNamespace:
		if len(r.namespace()) == 0 {
			return 7
		}

		if m.namespace != "" {
			if m.namespace != string(r.namespace()) {
				return 8
			}
		}
	}
	if m.name != "" && string(r.name()) != m.name {
		return 9
	}

	return 0
}

type MatchOption func(m *Matcher)

func WithName(namespace string, name string) MatchOption {
	return func(m *Matcher) {
		m.namespace = namespace
		m.name = name
	}
}

func WithNamespace(namespace string) MatchOption {
	return func(m *Matcher) {
		m.namespace = namespace
	}
}

func WithRealm(realm string) MatchOption {
	return func(m *Matcher) {
		m.realm = realm
	}
}

func NewMatcher(rm *meta.RESTMapping, opts ...MatchOption) (*Matcher, error) {
	m := &Matcher{
		realm: string(defaultRealm),
		rm:    rm,
	}
	m.Apply(opts...)
	return m, nil
}

var refSeparator = []byte{0}

func (m *Matcher) Apply(opts ...MatchOption) {
	for _, o := range opts {
		o(m)
	}
}

func (m *Matcher) scope() ([]byte, error) {
	switch m.rm.Scope {
	case meta.RESTScopeRoot:
		return clusterScope, nil
	case meta.RESTScopeNamespace:
		return namespaceScope, nil
	default:
		return nil, errors.New("invalid scope")
	}
}

func (m *Matcher) keyPrefix() ([]byte, error) {
	scope, err := m.scope()
	if err != nil {
		return nil, err
	}

	b := bytes.NewBuffer(make([]byte, 0, 256))
	b.Write([]byte(m.realm))
	b.Write(refSeparator)
	b.Write([]byte(scope))
	b.Write(refSeparator)
	b.Write([]byte(m.rm.Resource.Group))
	b.Write(refSeparator)
	b.Write([]byte(m.rm.Resource.Version))
	b.Write(refSeparator)
	b.Write([]byte(m.rm.Resource.Resource))
	b.Write(refSeparator)

	if m.namespace != "" {
		b.Write([]byte(m.namespace))
		b.Write(refSeparator)
	}

	if m.name != "" {
		// Exact match — seek key plus trailing separator (same as a
		// fromObject key). No sentinel needed.
		b.Write([]byte(m.name))
		b.Write(refSeparator)
	} else {
		// Range match — four-null sentinel makes the seek key larger
		// than any actual key in the range, so Seek lands at the first
		// matching key. Stripped for the HasPrefix check in Query.
		b.Write([]byte{0, 0, 0, 0})
	}
	return b.Bytes(), nil
}

var objectsBucketName = []byte("sticks.objects")

type DB struct {
	b          *bolt.DB
	serializer runtime.Serializer
	scheme     *runtime.Scheme

	watchInit sync.Once
	watcher   *Watcher
}

func Open(fp string, s *runtime.Scheme) (*DB, error) {
	b, err := bolt.Open(fp, 0600, nil)
	if err != nil {
		return nil, err
	}

	d := &DB{
		b:        b,
		scheme:   s,
		serializer: cbor.NewSerializer(s, s),
	}

	err = d.b.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(objectsBucketName)
		return err
	})
	return d, err
}

func (p *DB) notify(key []byte, typ watch.EventType) {
	p.getNotifier().fanOut(watchEvent{key: key, typ: typ})
}

func (p *DB) notifyWithObject(key []byte, typ watch.EventType, obj runtime.Object) {
	p.getNotifier().fanOut(watchEvent{key: key, typ: typ, staleObj: obj})
}

func (p *DB) getNotifier() *Watcher {
	p.watchInit.Do(func() {
		p.watcher = NewWatcher()
	})
	return p.watcher
}

func (p *DB) Create(rm *meta.RESTMapping, obj runtime.Object) error {
	var keyCopy []byte
	err := p.b.Update(func(tx *bolt.Tx) error {
		newMo, err := meta.Accessor(obj)
		if err != nil {
			return err
		}

		k, err := fromObject(rm, newMo)
		if err != nil {
			return err
		}

		_, err = p.txGet(tx, k)
		switch {
		case err == nil:
			return ErrAlreadyExists
		case !errors.Is(err, ErrDoesNotExist):
			return err
		}

		isNs := (rm.Scope == meta.RESTScopeNamespace)
		if err := validation.ValidateObjectMetaAccessor(newMo, isNs, path.ValidatePathSegmentName, field.NewPath("metadata")); err != nil {
			return err.ToAggregate()
		}

		newMo.SetCreationTimestamp(metav1.Now())
		newMo.SetSelfLink("")
		newMo.SetResourceVersion("")
		newMo.SetUID(uuid.NewUUID())
		newMo.SetManagedFields(nil)
		newMo.SetGeneration(0)
		newMo.SetGenerateName("")

		if err := p.txPut(tx, k, obj); err != nil {
			return err
		}
		keyCopy = bytes.Clone(k.key)
		return nil
	})
	if err == nil && keyCopy != nil {
		p.notify(keyCopy, watch.Added)
	}
	return err
}

func (p *DB) Update(k *Reference, newObject runtime.Object) error {
	var keyCopy []byte
	err := p.b.Update(func(tx *bolt.Tx) error {
		oldObject, err := p.txGet(tx, k)
		if err != nil {
			return err
		}

		oldMo, err := meta.Accessor(oldObject)
		if err != nil {
			return err
		}
		newMo, err := meta.Accessor(newObject)
		if err != nil {
			return err
		}

		if err := validation.ValidateObjectMetaAccessorUpdate(newMo, oldMo, field.NewPath("metadata")); err != nil {
			return err.ToAggregate()
		}

		if err := p.txPut(tx, k, newObject); err != nil {
			return err
		}
		keyCopy = bytes.Clone(k.key)
		return nil
	})
	if err == nil && keyCopy != nil {
		p.notify(keyCopy, watch.Modified)
	}
	return err
}

func (p *DB) Delete(k *Reference) error {
	var keyCopy []byte
	var objCopy runtime.Object
	err := p.b.Update(func(tx *bolt.Tx) error {
		oldObj, err := p.txGet(tx, k)
		if err != nil {
			return err
		}

		b := tx.Bucket(objectsBucketName)
		if b == nil {
			return ErrObjectsBucketNotFound
		}
		if err := b.Delete(k.key); err != nil {
			return err
		}

		keyCopy = bytes.Clone(k.key)
		objCopy = oldObj.DeepCopyObject()
		return nil
	})
	if err == nil && keyCopy != nil {
		p.notifyWithObject(keyCopy, watch.Deleted, objCopy)
	}
	return err
}

func (p *DB) Get(k *Reference) (runtime.Object, error) {
	var obj runtime.Object
	err := p.b.View(func(tx *bolt.Tx) error {
		var err error
		obj, err = p.txGet(tx, k)
		return err
	})
	return obj, err
}

func (p *DB) txPut(tx *bolt.Tx, k *Reference, obj runtime.Object) error {
	mo, err := meta.Accessor(obj)
	if err != nil {
		return err
	}

	b := tx.Bucket(objectsBucketName)
	if b == nil {
		return ErrObjectsBucketNotFound
	}
	nextResourceVersion, err := b.NextSequence()
	if err != nil {
		return err
	}
	mo.SetResourceVersion(strconv.FormatUint(nextResourceVersion, 10))
	mo.SetGeneration(mo.GetGeneration() + 1)

	buf := bytes.NewBuffer(make([]byte, 0, 512*1024))
	if err := p.serializer.Encode(obj, buf); err != nil {
		return err
	}

	return b.Put(k.key, buf.Bytes())
}

func (p *DB) txGet(tx *bolt.Tx, k *Reference) (runtime.Object, error) {
	b := tx.Bucket(objectsBucketName)
	if b == nil {
		return nil, ErrObjectsBucketNotFound
	}
	v := b.Get([]byte(k.key))
	if v == nil {
		return nil, ErrDoesNotExist
	}
	var u unstructured.Unstructured
	_, gvk, err := p.serializer.Decode(v, nil, &u)
	if err != nil {
		return nil, err
	}

	return p.scheme.ConvertToVersion(&u, gvk.GroupVersion())
}

func (p *DB) Single(m *Matcher) (*Reference, error) {
	refs, err := p.Query(m)
	if err != nil {
		return nil, err
	}

	if len(refs) != 1 {
		return nil, errors.New("expected exactly one match")
	}

	return refs[0], nil
}

func (p *DB) Query(m *Matcher) ([]*Reference, error) {
	bk, err := m.keyPrefix()
	if err != nil {
		return nil, err
	}

	// When name is specified, the prefix is an exact key (no sentinel).
	// For range scans, trim the 4-null sentinel.
	sentinelLen := 0
	if m.name == "" {
		sentinelLen = 4
	}
	var rangePrefix []byte
	if sentinelLen > 0 {
		rangePrefix = bk[:len(bk)-sentinelLen]
	} else {
		rangePrefix = bk
	}

	var refs []*Reference
	err = p.b.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(objectsBucketName)
		if b == nil {
			return ErrObjectsBucketNotFound
		}
		c := b.Cursor()
		for k, v := c.Seek(bk); k != nil && bytes.HasPrefix(k, rangePrefix); k, v = c.Next() {
			r, err := fromStorage(k, v)
			if err != nil {
				continue
			}
			if m.Matches(r) == 0 {
				refs = append(refs, r)
			}
		}
		return nil
	})
	return refs, err
}