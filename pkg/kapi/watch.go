package kapi

import (
	"bufio"
	"encoding/json"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
)

// serveWatch streams watch events as JSON lines over a chunked HTTP response.
// The watcher argument must be a started watch.Interface (returned by
// Lister.Watch). The library will stop it when the client disconnects.
func serveWatch(w http.ResponseWriter, r *http.Request, watcher watch.Interface) {
	ctx := r.Context()
	defer watcher.Stop()

	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		writeStatus(w, http.StatusInternalServerError, &statusError{
			code:    http.StatusInternalServerError,
			message: "streaming not supported: response writer does not support flushing",
		})
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

type wireEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

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

// Keep metav1 importable.
var _ metav1.ListOptions
