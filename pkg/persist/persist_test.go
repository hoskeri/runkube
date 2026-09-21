package persist

import (
	"bytes"
	"context"
	"path"
	"testing"
	"time"

	rawcbor "github.com/fxamacker/cbor/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/watch"
)

func fatal(t *testing.T, err error) {
	if err != nil {
		t.Fatal(err)
	}
}

var (
	sticksMapping = &meta.RESTMapping{
		Resource: schema.GroupVersionResource{
			Group:    "group.sticks.io",
			Version:  "v1",
			Resource: "sticks",
		},
		GroupVersionKind: schema.GroupVersionKind{
			Group:   "group.sticks.io",
			Version: "v1",
			Kind:    "Stick",
		},
		Scope: meta.RESTScopeNamespace,
	}

	carrotsMapping = &meta.RESTMapping{
		Resource: schema.GroupVersionResource{
			Group:    "group.sticks.io",
			Version:  "v1",
			Resource: "carrots",
		},
		GroupVersionKind: schema.GroupVersionKind{
			Group:   "group.sticks.io",
			Version: "v1",
			Kind:    "Carrot",
		},
		Scope: meta.RESTScopeRoot,
	}
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "group.sticks.io",
		Version: "v1",
		Kind:    "Carrot",
	}, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "group.sticks.io",
		Version: "v1",
		Kind:    "Stick",
	}, &unstructured.Unstructured{})
	return s
}

func randomLabels() map[string]string {
	m := map[string]string{}

	for range rand.IntnRange(2, 32) {
		randomKey := rand.String(rand.IntnRange(3, 8))
		randomValue := rand.String(rand.IntnRange(15, 62))
		m[randomKey] = randomValue
	}

	return m
}

func randomName() string {
	return rand.String(rand.IntnRange(2, 62))
}

func makeObject(m *meta.RESTMapping) runtime.Object {
	u := &unstructured.Unstructured{}
	u.SetUnstructuredContent(map[string]any{
		"apiVersion": m.GroupVersionKind.GroupVersion().String(),
		"kind":       m.GroupVersionKind.Kind,
		"metadata": map[string]any{
			"name": randomName(),
		},
		"spec": map[string]any{
			"data": rand.String(1024 * 768),
		},
	})
	if m.Scope == meta.RESTScopeNamespace {
		u.SetNamespace(randomName())
	}
	u.SetLabels(randomLabels())
	u.SetAnnotations(randomLabels())
	return u
}

func TestUnstructured(t *testing.T) {
	scm := runtime.NewScheme()
	serializer := cbor.NewSerializer(scm, scm)
	obj := makeObject(sticksMapping)
	b := bytes.NewBuffer(nil)
	if err := serializer.Encode(obj, b); err != nil {
		t.Fatalf("%#v", err)
	}

	o := struct {
		API      string `json:"apiVersion"`
		Kind     string `json:"kind"`
		Metadata struct {
			Name            string
			Namespace       string
			UID             string
			ResourceVersion string
			Generation      string
		} `json:"metadata"`
	}{}
	dm, err := rawcbor.DecOptions{
		FieldNameByteString: rawcbor.FieldNameByteStringAllowed,
		ByteStringToString:  rawcbor.ByteStringToStringAllowed,
	}.DecMode()

	err = dm.NewDecoder(bytes.NewBuffer(b.Bytes())).Decode(&o)
	u := &unstructured.Unstructured{}

	_, _, err = serializer.Decode(b.Bytes(), nil, u)
	if err != nil {
		t.Fatalf("%#v", err)
	}
}

func TestPod(t *testing.T) {
	scm := runtime.NewScheme()
	corev1.AddToScheme(scm)
	metav1.AddMetaToScheme(scm)
	serializer := cbor.NewSerializer(scm, scm)
	obj := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test",
		},
	}
	b := bytes.NewBuffer(nil)
	if err := serializer.Encode(obj, b); err != nil {
		t.Fatalf("%#v", err)
	}

	o := struct {
		API      string `json:"apiVersion"`
		Kind     string `json:"kind"`
		Metadata struct {
			Name            string
			Namespace       string
			UID             string
			ResourceVersion string
			Generation      string
		} `json:"metadata"`
	}{}
	dm, err := rawcbor.DecOptions{
		FieldNameByteString: rawcbor.FieldNameByteStringAllowed,
		ByteStringToString:  rawcbor.ByteStringToStringAllowed,
	}.DecMode()

	err = dm.NewDecoder(bytes.NewBuffer(b.Bytes())).Decode(&o)
	_ = &unstructured.Unstructured{}

	_, _, err = serializer.Decode(b.Bytes(), nil, obj)
	if err != nil {
		t.Fatalf("%#v", err)
	}
}

func TestReference(t *testing.T) {
	for _, rm := range []*meta.RESTMapping{
		sticksMapping, carrotsMapping,
	} {
		t.Run(rm.GroupVersionKind.String(), func(t *testing.T) {
			obj := makeObject(sticksMapping)
			mo, err := meta.Accessor(obj)
			if err != nil {
				t.Fatalf("meta.Accessor %#v", err)
			}

			r, err := fromObject(rm, mo)
			if err != nil {
				t.Fatalf("persist.FromObject %#v", err)
			}

			m, err := NewMatcher(rm, WithName(mo.GetNamespace(), mo.GetName()), WithRealm(string(defaultRealm)))
			if err != nil {
				t.Fatalf("persist.NewMatcher %#v", err)
			}

			if m.Matches(r) != 0 {
				t.Fatalf("m.Matches failed")
			}
		})
	}
}

func TestCreateDuplicate(t *testing.T) {
	dbp := path.Join(t.TempDir(), "test.db")
	db, err := Open(dbp, testScheme())
	fatal(t, err)
	defer db.b.Close()

	obj := makeObject(sticksMapping)
	if err := db.Create(sticksMapping, obj); err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Second create with the same name should fail.
	if err := db.Create(sticksMapping, obj); err != ErrAlreadyExists {
		t.Fatalf("second create: expected ErrAlreadyExists, got %v", err)
	}
}

func TestDelete(t *testing.T) {
	dbp := path.Join(t.TempDir(), "test.db")
	db, err := Open(dbp, testScheme())
	fatal(t, err)
	defer db.b.Close()

	obj := makeObject(sticksMapping)
	fatal(t, db.Create(sticksMapping, obj))

	m, err := NewMatcher(sticksMapping)
	fatal(t, err)
	refs, err := db.Query(m)
	fatal(t, err)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}

	fatal(t, db.Delete(refs[0]))

	// After Delete, Query should return no results.
	refs2, err := db.Query(m)
	fatal(t, err)
	if len(refs2) != 0 {
		t.Fatalf("expected 0 refs after delete, got %d", len(refs2))
	}

	// Get on deleted ref should fail.
	_, err = db.Get(refs[0])
	if err != ErrDoesNotExist {
		t.Fatalf("expected ErrDoesNotExist, got %v", err)
	}
}

func TestDeleteNonexistent(t *testing.T) {
	dbp := path.Join(t.TempDir(), "test.db")
	db, err := Open(dbp, testScheme())
	fatal(t, err)
	defer db.b.Close()

	obj := makeObject(sticksMapping)
	fatal(t, db.Create(sticksMapping, obj))

	m, err := NewMatcher(sticksMapping)
	fatal(t, err)
	refs, err := db.Query(m)
	fatal(t, err)

	fatal(t, db.Delete(refs[0]))
	if err := db.Delete(refs[0]); err != ErrDoesNotExist {
		t.Fatalf("expected ErrDoesNotExist, got %v", err)
	}
}

func TestQueryRange(t *testing.T) {
	// Verify that Query only returns keys within the matching GVR range
	// and doesn't produce false positives from other GVRs.
	dbp := path.Join(t.TempDir(), "test.db")
	db, err := Open(dbp, testScheme())
	fatal(t, err)
	defer db.b.Close()

	// Create one of each.
	carrot := makeObject(carrotsMapping)
	stick := makeObject(sticksMapping)
	fatal(t, db.Create(carrotsMapping, carrot))
	fatal(t, db.Create(sticksMapping, stick))

	// Query for sticks — should get exactly 1.
	mSticks, err := NewMatcher(sticksMapping)
	fatal(t, err)
	refs, err := db.Query(mSticks)
	fatal(t, err)
	if len(refs) != 1 {
		t.Fatalf("sticks query: expected 1, got %d", len(refs))
	}

	// Query for carrots — should get exactly 1.
	mCarrots, err := NewMatcher(carrotsMapping)
	fatal(t, err)
	refs, err = db.Query(mCarrots)
	fatal(t, err)
	if len(refs) != 1 {
		t.Fatalf("carrots query: expected 1, got %d", len(refs))
	}
}

func TestCRUD(t *testing.T) {
	dbp := path.Join(t.TempDir(), "test.db")
	db, err := Open(dbp, testScheme())
	fatal(t, err)
	defer db.b.Close()

	t.Run("CREATE UPDATE", func(t *testing.T) {
		for _, rm := range []*meta.RESTMapping{
			sticksMapping, carrotsMapping,
		} {

			t.Run(rm.GroupVersionKind.String(), func(t *testing.T) {
				obj := makeObject(rm)
				if err := db.Create(rm, obj); err != nil {
					t.Fatalf("err: %v", err)
				}

				m, err := NewMatcher(rm)
				if err != nil {
					t.Fatalf("err: %v", err)
				}

				refs, err := db.Query(m)
				if err != nil {
					t.Fatalf("err: %v", err)
				}

				gotObj, err := db.Get(refs[0])
				if err != nil {
					t.Fatal(err)
				}

				mo, err := meta.Accessor(gotObj)
				if err != nil {
					t.Fatal(err)
				}

				t.Logf("%v", mo.GetUID())
			})
		}
	})
}

// ── Watch tests ────────────────────────────────────────────

func TestWatchCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dbp := path.Join(t.TempDir(), "test.db")
	db, err := Open(dbp, testScheme())
	fatal(t, err)
	defer db.b.Close()

	m, err := NewMatcher(sticksMapping)
	fatal(t, err)

	w, err := db.Watch(ctx, m, WatchOptions{})
	fatal(t, err)
	defer w.Stop()

	// Create an object.
	obj := makeObject(sticksMapping)
	mo, err := meta.Accessor(obj)
	fatal(t, err)
	mo.SetName("watchtest")
	mo.SetNamespace("testns")
	if err := db.Create(sticksMapping, obj); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Should receive an Added event.
	select {
	case evt, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("result channel closed")
		}
		if evt.Type != watch.Added {
			t.Fatalf("expected Added, got %v", evt.Type)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for Added event")
	}
}

func TestWatchUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dbp := path.Join(t.TempDir(), "test.db")
	db, err := Open(dbp, testScheme())
	fatal(t, err)
	defer db.b.Close()

	obj := makeObject(sticksMapping)
	mo, err := meta.Accessor(obj)
	fatal(t, err)
	mo.SetName("watchupdate")
	mo.SetNamespace("testns")
	fatal(t, db.Create(sticksMapping, obj))

	m, err := NewMatcher(sticksMapping, WithName("testns", "watchupdate"))
	fatal(t, err)

	w, err := db.Watch(ctx, m, WatchOptions{})
	fatal(t, err)
	defer w.Stop()

	// Consume the bookmark Added event from the bookmark scan.
	select {
	case <-w.ResultChan():
	case <-ctx.Done():
		t.Fatal("timeout waiting for bookmark event")
	}

	// Update the object.
	mo, err = meta.Accessor(obj)
	fatal(t, err)
	mo.SetLabels(map[string]string{"updated": "true"})
	refs, err := db.Query(m)
	fatal(t, err)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	fatal(t, db.Update(refs[0], obj))

	// Should receive a Modified event.
	select {
	case evt, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("result channel closed")
		}
		if evt.Type != watch.Modified {
			t.Fatalf("expected Modified, got %v", evt.Type)
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for Modified event")
	}
}

func TestWatchDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dbp := path.Join(t.TempDir(), "test.db")
	db, err := Open(dbp, testScheme())
	fatal(t, err)
	defer db.b.Close()

	obj := makeObject(sticksMapping)
	mo, err := meta.Accessor(obj)
	fatal(t, err)
	mo.SetName("watchdelete")
	mo.SetNamespace("testns")
	fatal(t, db.Create(sticksMapping, obj))

	m, err := NewMatcher(sticksMapping, WithName("testns", "watchdelete"))
	fatal(t, err)

	w, err := db.Watch(ctx, m, WatchOptions{})
	fatal(t, err)
	defer w.Stop()

	// Consume bookmark Added.
	select {
	case <-w.ResultChan():
	case <-ctx.Done():
		t.Fatal("timeout waiting for bookmark event")
	}

	// Delete.
	refs, err := db.Query(m)
	fatal(t, err)
	fatal(t, db.Delete(refs[0]))

	// Should receive a Deleted event with the pre-deletion object.
	select {
	case evt, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("result channel closed")
		}
		if evt.Type != watch.Deleted {
			t.Fatalf("expected Deleted, got %v", evt.Type)
		}
		if evt.Object == nil {
			t.Fatal("Deleted event has nil object")
		}
		mo, err := meta.Accessor(evt.Object)
		fatal(t, err)
		if mo.GetName() != "watchdelete" {
			t.Fatalf("expected name watchdelete, got %q", mo.GetName())
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for Deleted event")
	}
}

func TestWatchFiltering(t *testing.T) {
	// Verify that a watcher on sticks only receives sticks events,
	// not carrot events.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dbp := path.Join(t.TempDir(), "test.db")
	db, err := Open(dbp, testScheme())
	fatal(t, err)
	defer db.b.Close()

	m, err := NewMatcher(sticksMapping)
	fatal(t, err)

	w, err := db.Watch(ctx, m, WatchOptions{})
	fatal(t, err)
	defer w.Stop()

	// Create a carrot (should not appear on sticks watcher).
	carrot := makeObject(carrotsMapping)
	cmo, err := meta.Accessor(carrot)
	fatal(t, err)
	cmo.SetName("acrocarrot")
	fatal(t, db.Create(carrotsMapping, carrot))

	// Create a stick (should appear).
	stick := makeObject(sticksMapping)
	smo, err := meta.Accessor(stick)
	fatal(t, err)
	smo.SetName("b-stick")
	smo.SetNamespace("ns1")
	fatal(t, db.Create(sticksMapping, stick))

	// Consume events. Expect one Added for the stick only.
	select {
	case evt, ok := <-w.ResultChan():
		if !ok {
			t.Fatal("result channel closed")
		}
		if evt.Type != watch.Added {
			t.Fatalf("expected Added, got %v", evt.Type)
		}
		mo, err := meta.Accessor(evt.Object)
		fatal(t, err)
		if mo.GetName() != "b-stick" {
			t.Fatalf("expected name 'b-stick', got %q", mo.GetName())
		}
	case <-ctx.Done():
		t.Fatal("timeout waiting for Added event")
	}

	// There should be no second event within a short window.
	select {
	case evt := <-w.ResultChan():
		t.Fatalf("unexpected second event: %v", evt)
	default:
		// OK — no extra events.
	}
}