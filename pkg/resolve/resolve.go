package resolve

import (
	"context"
	"fmt"
	"iter"
	"sync"
)

type Valuer interface {
	Key() string
	Value(ctx context.Context) (string, error)
}

type ValueError struct {
	inner error
}

func (v ValueError) Error() string {
	return fmt.Sprintf("ValueError %v", v.inner)
}

type Resolver struct {
	errMutex sync.Mutex
	err      error
}

func (r *Resolver) setError(err error) {
	r.errMutex.Lock()
	defer r.errMutex.Unlock()
	if r.err != nil {
		return
	}

	r.err = err
}

func (r *Resolver) Resolve(ctx context.Context, in iter.Seq[any]) iter.Seq[string] {
	return func(yield func(string) bool) {
		for i := range in {
			var y string
			var err error
			switch x := i.(type) {
			case Valuer:
				if y, err = r.resolveValue(ctx, x); err != nil {
					r.setError(err)
					break
				}
			case string:
				y = x
			}

			if !yield(y) {
				return
			}
		}
	}
}

func (r *Resolver) resolveValue(ctx context.Context, v Valuer) (string, error) {
	x, err := v.Value(ctx)
	if err != nil {
		return "", ValueError{
			inner: err,
		}
	}
	return x, nil
}
