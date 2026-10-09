package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// sseWriter streams JSON events as text/event-stream and flushes after every one.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

// newSSE commits a 200 answer and flushes its headers, so the client sees the stream open at once. A writer that
// cannot flush still gets every event, only later.
func newSSE(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx would otherwise hold the events back
	w.WriteHeader(http.StatusOK)
	s := &sseWriter{w: w, rc: http.NewResponseController(w)}
	_ = s.rc.Flush()
	return s
}

func (s *sseWriter) send(v any) {
	data, _ := json.Marshal(v) // the events are plain structs, which always marshal
	_, _ = fmt.Fprintf(s.w, "data: %s\n\n", data)
	_ = s.rc.Flush()
}
