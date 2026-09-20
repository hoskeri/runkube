package kapi

import (
	"bufio"
	"encoding/json"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
)

// serveWatch streams watch events as JSON lines over a chunked HTTP response.
// It calls the user's WatchHandler, then encodes each event as:
//
//	{"type":"ADDED","object":{...}}
//	{"type":"MODIFIED","object":{...}}
//	...
//
// The handler blocks until the watch channel closes, the client disconnects,
// or the request context is cancelled.
func serveWatch(w http.ResponseWriter, r *http.Request, fn WatchHandler, ns string, opts metav1.ListOptions) {
	ctx := r.Context()

	watcher, err := fn(ctx, ns, opts)
	if err != nil {
		writeError(w, err)
		return
	}
	defer watcher.Stop()

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeStatus(w, http.StatusInternalServerError, errNotFlushable())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)

	for {
		select {
		case evt, ok := <-watcher.ResultChan():
			if !ok {
				return
			}
			if err := enc.Encode(newWireEvent(evt)); err != nil {
				return
			}
			bw.Flush()
			flusher.Flush()

		case <-ctx.Done():
			return
		}
	}
}

// wireEvent is the on-wire JSON structure for a watch event.
type wireEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// newWireEvent builds a wireEvent from a watch.Event.
func newWireEvent(evt watch.Event) wireEvent {
	var raw json.RawMessage
	if evt.Object != nil {
		if data, err := json.Marshal(evt.Object); err == nil {
			raw = data
		}
	}
	return wireEvent{
		Type:   string(evt.Type),
		Object: raw,
	}
}

func errNotFlushable() error {
	return &statusError{
		code:    http.StatusInternalServerError,
		message: "streaming not supported: response writer does not support flushing",
	}
}

// Keep runtime.Object importable.
var _ runtime.Object = nil