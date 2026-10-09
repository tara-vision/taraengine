package llamatest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type chatRequest struct {
	Model              string           `json:"model"`
	Stream             bool             `json:"stream"`
	Tools              []map[string]any `json:"tools"`
	ChatTemplateKwargs map[string]any   `json:"chat_template_kwargs"`
}

func (req chatRequest) thinking() bool {
	v, ok := req.ChatTemplateKwargs["enable_thinking"]
	return !ok || v != false
}

var timings = map[string]any{
	"cache_n": 0, "prompt_n": 46, "prompt_ms": 5.267, "prompt_per_token_ms": 0.1145, "prompt_per_second": 8733.6,
	"predicted_n": 12, "predicted_ms": 3.095, "predicted_per_token_ms": 0.2814, "predicted_per_second": 3554.1,
}

var usage = map[string]any{
	"completion_tokens": 12, "prompt_tokens": 46, "total_tokens": 58,
	"prompt_tokens_details": map[string]int{"cached_tokens": 0},
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	m, ok := s.models[req.Model]
	if !ok {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "model '"+req.Model+"' not found", "invalid_request_error")
		return
	}
	if m.status != "loaded" {
		s.evictFor(req.Model)
		m.status, m.failed, m.loadedAt = "loaded", false, time.Now()
	}
	s.mu.Unlock()
	if req.Stream {
		s.streamChat(w, req)
		return
	}
	message := map[string]any{"role": "assistant", "content": "Hello from " + req.Model}
	if req.thinking() {
		message["reasoning_content"] = "thinking about " + req.Model
	}
	if len(req.Tools) > 0 {
		message["content"] = ""
		message["tool_calls"] = []map[string]any{{
			"id": "call_1", "type": "function",
			"function": map[string]string{"name": "get_weather", "arguments": `{"city":"Ohrid"}`},
		}}
	}
	writeJSON(w, map[string]any{
		"id": "chatcmpl-fake", "object": "chat.completion", "created": 1, "model": req.Model,
		"system_fingerprint": "bTEST-fake",
		"choices":            []map[string]any{{"index": 0, "finish_reason": "stop", "message": message}},
		"usage":              usage, "timings": timings,
	})
}

func (s *Server) streamChat(w http.ResponseWriter, req chatRequest) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(chunk map[string]any) {
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(s.opts.ChatDelay)
	}
	chunk := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 1, "model": req.Model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}}
	}
	send(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	if req.thinking() {
		send(chunk(map[string]any{"reasoning_content": "thinking"}, nil))
	}
	if len(req.Tools) > 0 {
		send(chunk(map[string]any{"tool_calls": []map[string]any{{"index": 0, "id": "call_1", "type": "function",
			"function": map[string]string{"name": "get_weather", "arguments": `{"city":"Ohrid"}`}}}}, nil))
	} else {
		for _, word := range []string{"Hello", " from ", req.Model} {
			send(chunk(map[string]any{"content": word}, nil))
		}
	}
	send(chunk(map[string]any{}, "stop"))
	send(map[string]any{"id": "chatcmpl-fake", "object": "chat.completion.chunk", "created": 1, "model": req.Model,
		"choices": []any{}, "usage": usage, "timings": timings})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
