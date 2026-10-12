package llamatest

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options script the fake.
type Options struct {
	APIKey     string
	PresetPath string        // when set, GET /models?reload=1 re-reads the sections
	ModelsMax  int           // 0 means unlimited, as the router's 0 does
	LoadDelay  time.Duration // loading -> loaded (or failed)
	ChatDelay  time.Duration // between streamed chunks
	// UnloadDelay keeps a model listed as loaded for this long after POST /models/unload has answered, as
	// the real router does for about 80 ms; 0 unloads before the answer.
	UnloadDelay time.Duration
}

type model struct {
	id       string
	path     string
	keys     map[string]string
	status   string
	exitCode int
	failed   bool
	loadedAt time.Time
}

// Server is the fake router. Like llama-server it answers CORS for every origin: any answer to a request
// with an Origin header approves that origin, and a preflight is approved before the key is checked.
type Server struct {
	opts     Options
	srv      *httptest.Server
	mu       sync.Mutex
	models   map[string]*model
	order    []string
	failLoad map[string]int
	hangLoad map[string]bool
	requests []string
}

// New starts the fake; it is closed when the test ends.
func New(t testing.TB, opts Options) *Server {
	s := &Server{opts: opts, models: map[string]*model{}, failLoad: map[string]int{}, hangLoad: map[string]bool{}}
	if opts.PresetPath != "" {
		s.readPresets()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.auth(s.health))
	mux.HandleFunc("GET /models", s.auth(s.list))
	mux.HandleFunc("GET /v1/models", s.auth(s.list))
	mux.HandleFunc("POST /models/load", s.auth(s.load))
	mux.HandleFunc("POST /models/unload", s.auth(s.unload))
	mux.HandleFunc("GET /props", s.auth(s.props))
	mux.HandleFunc("POST /v1/chat/completions", s.auth(s.chat))
	s.srv = httptest.NewServer(cors(mux))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the fake's base URL.
func (s *Server) URL() string { return s.srv.URL }

// Close stops the fake before the test ends.
func (s *Server) Close() { s.srv.Close() }

// AddModel registers an unloaded model, as a preset section would.
func (s *Server) AddModel(id, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.add(id, map[string]string{"model": path})
}

func (s *Server) add(id string, keys map[string]string) {
	if _, ok := s.models[id]; !ok {
		s.order = append(s.order, id)
	}
	s.models[id] = &model{id: id, path: keys["model"], keys: keys, status: "unloaded"}
}

// FailLoad makes the next loads of id fail with the exit code. It scripts POST /models/load only: a model a
// chat request brings up (an autoload) loads as usual.
func (s *Server) FailLoad(id string, exitCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failLoad[id] = exitCode
}

// HangLoad makes loads of id stay in loading until an unload or a reload stops them. It scripts POST
// /models/load only: a model a chat request brings up (an autoload) loads as usual.
func (s *Server) HangLoad(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hangLoad[id] = true
}

// Status is the model's status value, "" when unknown.
func (s *Server) Status(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.models[id]; ok {
		return m.status
	}
	return ""
}

// SetStatus forces a status, for example sleeping.
func (s *Server) SetStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.models[id]; ok {
		m.status = status
	}
}

// SetFailed ends the model the way the router lists a load that failed: unloaded, failed, with the exit code. A
// load still pending for it never comes up.
func (s *Server) SetFailed(id string, exitCode int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.models[id]; ok {
		m.status, m.failed, m.exitCode = "unloaded", true, exitCode
	}
}

// Requests lists every authenticated request as "METHOD path?query".
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// cors answers CORS the way llama-server does.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Allow-Headers", "*")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.opts.APIKey {
			writeError(w, http.StatusUnauthorized, "Invalid API Key", "authentication_error")
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
		s.mu.Unlock()
		next(w, r)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) readPresets() {
	data, err := os.ReadFile(s.opts.PresetPath)
	if err != nil {
		return
	}
	sections := ParseINI(data)
	seen := map[string]bool{}
	// Sorted, so the listing never depends on map order. The real router rewrites a colon in a section
	// name and the engine never writes one, so such a section is dropped: a missed mapping then shows
	// up as an unknown model instead of passing by luck.
	for _, id := range slices.Sorted(maps.Keys(sections)) {
		if id == "*" || strings.Contains(id, ":") {
			continue
		}
		keys := sections[id]
		seen[id] = true
		if m, ok := s.models[id]; ok {
			if running(m.status) && !maps.Equal(m.keys, keys) {
				m.status = "unloaded" // the router stops a running model whose section changed
			}
			m.keys, m.path = keys, keys["model"]
			continue
		}
		s.add(id, keys)
	}
	kept := s.order[:0:0]
	for _, id := range s.order {
		if seen[id] {
			kept = append(kept, id)
		} else {
			delete(s.models, id)
		}
	}
	s.order = kept
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Query().Get("reload") == "1" && s.opts.PresetPath != "" {
		s.readPresets()
	}
	data := make([]map[string]any, 0, len(s.order))
	for _, id := range s.order {
		m := s.models[id]
		status := map[string]any{
			"value": m.status, "args": []string{"llama-server", "--alias", id}, "preset": renderSection(id, m.keys),
		}
		if m.failed {
			status["failed"] = true
			status["exit_code"] = m.exitCode
		}
		data = append(data, map[string]any{
			"id": id, "object": "model", "owned_by": "llamacpp", "status": status, "source": "preset", "can_remove": false,
			"architecture": map[string]any{"input_modalities": []string{"text"}, "output_modalities": []string{"text"}},
		})
	}
	writeJSON(w, map[string]any{"data": data, "object": "list"})
}

func (s *Server) load(w http.ResponseWriter, r *http.Request) {
	id := modelField(r)
	s.mu.Lock()
	m, ok := s.models[id]
	switch {
	case !ok:
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "File Not Found", "not_found_error")
		return
	case running(m.status):
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "model is already running", "invalid_request_error")
		return
	}
	m.status, m.failed = "loading", false
	s.mu.Unlock()
	writeJSON(w, map[string]bool{"success": true})
	go s.finishLoad(id)
}

// running reports whether the router has a process for a model in that status: loading, loaded or asleep.
func running(status string) bool {
	return status == "loading" || status == "loaded" || status == "sleeping"
}

// finishLoad ends a pending load after the load delay, unless the model is no longer loading by then: a reload may
// have dropped it, an unload, a reload or SetFailed may have stopped it, and a chat request may have brought it up.
func (s *Server) finishLoad(id string) {
	time.Sleep(s.opts.LoadDelay)
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[id]
	if !ok || m.status != "loading" || s.hangLoad[id] {
		return
	}
	if code, fail := s.failLoad[id]; fail {
		m.status, m.failed, m.exitCode = "unloaded", true, code
		return
	}
	s.evictFor(id)
	m.status, m.loadedAt = "loaded", time.Now()
}

// evictFor unloads the oldest resident models so that id fits under ModelsMax.
func (s *Server) evictFor(id string) {
	if s.opts.ModelsMax <= 0 {
		return
	}
	var resident []*model
	for _, other := range s.models {
		if other.id != id && (other.status == "loaded" || other.status == "sleeping") {
			resident = append(resident, other)
		}
	}
	sort.Slice(resident, func(a, b int) bool { return resident[a].loadedAt.Before(resident[b].loadedAt) })
	for len(resident) >= s.opts.ModelsMax {
		resident[0].status = "unloaded"
		resident = resident[1:]
	}
}

func (s *Server) unload(w http.ResponseWriter, r *http.Request) {
	id := modelField(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[id]
	switch {
	case !ok:
		writeError(w, http.StatusBadRequest, "model is not found", "invalid_request_error")
		return
	case m.status == "loading":
		m.status = "unloaded" // the router kills a model that is still loading at once
	case m.status != "loaded" && m.status != "sleeping":
		writeError(w, http.StatusBadRequest, "model is not running", "invalid_request_error")
		return
	case s.opts.UnloadDelay > 0:
		go s.finishUnload(id)
	default:
		m.status = "unloaded"
	}
	writeJSON(w, map[string]bool{"success": true})
}

// finishUnload stops the model once the unload delay has passed, unless it is loading again by then.
func (s *Server) finishUnload(id string) {
	time.Sleep(s.opts.UnloadDelay)
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.models[id]; ok && (m.status == "loaded" || m.status == "sleeping") {
		m.status = "unloaded"
	}
}

func (s *Server) props(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("model")
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.models[id]
	if !ok || (m.status != "loaded" && m.status != "sleeping") {
		writeError(w, http.StatusBadRequest, "model '"+id+"' not found", "invalid_request_error")
		return
	}
	ctx, _ := strconv.Atoi(firstOf(m.keys, "c", "ctx-size"))
	if ctx == 0 {
		ctx = 4096
	}
	writeJSON(w, map[string]any{
		"default_generation_settings": map[string]any{"n_ctx": ctx},
		"model_path":                  m.path,
		"model_alias":                 id,
		"chat_template":               "{% if enable_thinking %}<think>{% endif %}",
		"chat_template_caps": map[string]bool{
			"supports_tools": true, "supports_tool_calls": true, "supports_reasoning_effort": false,
		},
		"modalities":  map[string]bool{"vision": false, "video": false, "audio": false},
		"is_sleeping": m.status == "sleeping",
		"build_info":  "bTEST-fake",
	})
}

func modelField(r *http.Request) string {
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.Model
}

func firstOf(keys map[string]string, names ...string) string {
	for _, n := range names {
		if v, ok := keys[n]; ok {
			return v
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, v any) { writeBody(w, http.StatusOK, v) }

func writeError(w http.ResponseWriter, code int, msg, typ string) {
	writeBody(w, code, map[string]any{"error": map[string]any{"code": code, "message": msg, "type": typ}})
}

// writeBody sends v as JSON with no trailing newline, byte for byte as llama-server does.
func writeBody(w http.ResponseWriter, code int, v any) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}
