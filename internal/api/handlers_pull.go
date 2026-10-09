package api

import (
	"net/http"

	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/pkg/api"
)

// hintRef lists what a pull reference may be.
const hintRef = "a catalog name, hf://owner/repo/file.gguf, or an absolute .gguf path"

// pull starts a pull and answers with its progress as a stream of events, or with one JSON reply when ?wait=1
// asks for it. What the server will not start is answered with a status of its own in both modes; only a failure
// during the pull comes as an error event in the stream, or as 502 for a caller that waits.
func (s *Server) pull(w http.ResponseWriter, r *http.Request) {
	var req api.PullRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ref, err := pull.ParseRef(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), hintRef)
		return
	}
	p, refused := s.admit(ref)
	if refused != nil {
		refused.write(w)
		return
	}
	defer p.release()
	if r.URL.Query().Get("wait") != "1" {
		stream := newSSE(w)
		_ = s.runPull(r.Context(), p, func(e api.PullEvent) { stream.send(e) })
		return
	}
	if err := s.runPull(r.Context(), p, func(api.PullEvent) {}); err != nil {
		writeError(w, http.StatusBadGateway, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "done", "model": ref.Name})
}
