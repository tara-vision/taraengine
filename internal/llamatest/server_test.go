package llamatest

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func get(t *testing.T, s *Server, path, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.URL()+path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func post(t *testing.T, s *Server, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.URL()+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func statusOf(t *testing.T, body, id string) map[string]any {
	t.Helper()
	var list struct {
		Data []struct {
			ID     string         `json:"id"`
			Status map[string]any `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	for _, m := range list.Data {
		if m.ID == id {
			return m.Status
		}
	}
	return nil
}

//nolint:gocyclo // one linear lifecycle scenario: every step starts from the state the step before it left
func TestFakeRouterLifecycle(t *testing.T) {
	s := New(t, Options{APIKey: "k", LoadDelay: 20 * time.Millisecond, ModelsMax: 1})
	s.AddModel("a", "/m/a.gguf")
	s.AddModel("b", "/m/b.gguf")
	s.FailLoad("broken", 1)
	s.AddModel("broken", "/m/broken.gguf")

	if code, _ := get(t, s, "/models", ""); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	if code, body := get(t, s, "/health", "k"); code != 200 || body != `{"status":"ok"}` {
		t.Fatalf("health: %d %s", code, body)
	}
	if code, body := post(t, s, "/models/load", `{"model":"nope"}`); code != 404 || !strings.Contains(body, "File Not Found") {
		t.Fatalf("unknown load: %d %s", code, body)
	}
	if code, body := post(t, s, "/models/load", `{"model":"a"}`); code != 200 || body != `{"success":true}` {
		t.Fatalf("load: %d %s", code, body)
	}
	if st := s.Status("a"); st != "loading" {
		t.Fatalf("right after load: %s", st)
	}
	time.Sleep(50 * time.Millisecond)
	if st := s.Status("a"); st != "loaded" {
		t.Fatalf("after the delay: %s", st)
	}
	if code, body := get(t, s, "/props?model=a", "k"); code != 200 || !strings.Contains(body, `"n_ctx"`) {
		t.Fatalf("props: %d %s", code, body)
	}
	if code, _ := get(t, s, "/props?model=b", "k"); code != 400 {
		t.Fatalf("props of an unloaded model must be 400, got %d", code)
	}

	post(t, s, "/models/load", `{"model":"b"}`)
	time.Sleep(50 * time.Millisecond)
	if s.Status("a") != "unloaded" || s.Status("b") != "loaded" {
		t.Fatalf("models-max 1 must evict a: a=%s b=%s", s.Status("a"), s.Status("b"))
	}

	post(t, s, "/models/load", `{"model":"broken"}`)
	time.Sleep(50 * time.Millisecond)
	_, body := get(t, s, "/models", "k")
	st := statusOf(t, body, "broken")
	if st["value"] != "unloaded" || st["failed"] != true || st["exit_code"].(float64) != 1 {
		t.Fatalf("failed load shape: %v", st)
	}

	if code, body := post(t, s, "/models/unload", `{"model":"a"}`); code != 400 || !strings.Contains(body, "not running") {
		t.Fatalf("unload of a stopped model: %d %s", code, body)
	}
	if code, _ := post(t, s, "/models/unload", `{"model":"b"}`); code != 200 || s.Status("b") != "unloaded" {
		t.Fatalf("unload b: %d %s", code, s.Status("b"))
	}
	s.SetStatus("b", "sleeping")
	if code, _ := post(t, s, "/models/unload", `{"model":"b"}`); code != 200 {
		t.Fatalf("unload of a sleeping model must succeed, got %d", code)
	}
	if reqs := s.Requests(); len(reqs) == 0 || !strings.HasPrefix(reqs[0], "GET /health") {
		t.Fatalf("requests not recorded (the unauthenticated GET /models must not be): %v", reqs)
	}
}

func TestFakeRouterReloadFollowsThePresetFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.ini")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("[*]\njinja = true\n\n[one]\nmodel = /m/one.gguf\nc = 4096\n")
	s := New(t, Options{APIKey: "k", PresetPath: path})
	_, body := get(t, s, "/models", "k")
	if st := statusOf(t, body, "one"); st == nil || !strings.Contains(st["preset"].(string), "ctx-size = 4096") {
		t.Fatalf("initial preset: %v", st)
	}
	write("[*]\njinja = true\n\n[one]\nmodel = /m/one.gguf\nc = 8192\n\n[two]\nmodel = /m/two.gguf\n")
	_, body = get(t, s, "/models?reload=1", "k")
	if st := statusOf(t, body, "one"); !strings.Contains(st["preset"].(string), "ctx-size = 8192") {
		t.Fatalf("preset not refreshed: %v", st)
	}
	if st := statusOf(t, body, "two"); st == nil || st["value"] != "unloaded" {
		t.Fatalf("new section not listed: %v", st)
	}
	write("[*]\njinja = true\n\n[two]\nmodel = /m/two.gguf\n")
	_, body = get(t, s, "/models?reload=1", "k")
	if st := statusOf(t, body, "one"); st != nil {
		t.Fatal("removed section still listed")
	}
}

func TestFakeRouterChat(t *testing.T) {
	s := New(t, Options{APIKey: "k", ChatDelay: 5 * time.Millisecond})
	s.AddModel("a", "/m/a.gguf")
	code, body := post(t, s, "/v1/chat/completions", `{"model":"a","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(body, `"timings"`) || !strings.Contains(body, `"reasoning_content"`) {
		t.Fatalf("non-stream: %d %s", code, body)
	}
	if s.Status("a") != "loaded" {
		t.Fatal("chat must autoload the model")
	}
	_, body = post(t, s, "/v1/chat/completions", `{"model":"a","chat_template_kwargs":{"enable_thinking":false},"messages":[]}`)
	if strings.Contains(body, `"reasoning_content"`) {
		t.Fatal("thinking off must drop reasoning_content")
	}
	_, body = post(t, s, "/v1/chat/completions", `{"model":"a","messages":[],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, "get_weather") {
		t.Fatalf("tools: %s", body)
	}
	if code, body := post(t, s, "/v1/chat/completions", `{"model":"nope","messages":[]}`); code != 400 || !strings.Contains(body, "not found") {
		t.Fatalf("unknown model: %d %s", code, body)
	}

	req, _ := http.NewRequest(http.MethodPost, s.URL()+"/v1/chat/completions", strings.NewReader(`{"model":"a","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			lines = append(lines, sc.Text())
		}
	}
	if len(lines) < 4 || lines[len(lines)-1] != "data: [DONE]" || !strings.Contains(lines[len(lines)-2], `"timings"`) || !strings.Contains(lines[len(lines)-2], `"choices":[]`) {
		t.Fatalf("stream shape: %v", lines)
	}
}

func TestParseINI(t *testing.T) {
	got := ParseINI([]byte("# comment\n[*]\njinja = true\n\n[a:1]\nmodel = /m/a.gguf\nc = 4096\nbad line\n"))
	if got["*"]["jinja"] != "true" || got["a:1"]["model"] != "/m/a.gguf" || got["a:1"]["c"] != "4096" || len(got) != 2 {
		t.Fatalf("parsed %v", got)
	}
}

func idsOf(t *testing.T, body string) []string {
	t.Helper()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	ids := make([]string, 0, len(list.Data))
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	return ids
}

// The real router rewrites a colon in a section name and the engine never writes one, so the fake drops
// such a section: a missed colon mapping then shows up as an unknown model.
func TestFakeRouterIgnoresSectionsWithAColon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.ini")
	writePresetFile(t, path, "[*]\njinja = true\n\n[a:1]\nmodel = /m/a-colon.gguf\n\n[a-1]\nmodel = /m/a-dash.gguf\n")
	s := New(t, Options{APIKey: "k", PresetPath: path})
	_, body := get(t, s, "/models", "k")
	if ids := idsOf(t, body); len(ids) != 1 || ids[0] != "a-1" {
		t.Fatalf("only a-1 may be listed, got %v", ids)
	}
	if code, body := post(t, s, "/models/load", `{"model":"a:1"}`); code != http.StatusNotFound || !strings.Contains(body, "File Not Found") {
		t.Fatalf("a colon section must not be loadable: %d %s", code, body)
	}
	writePresetFile(t, path, "[a:1]\nmodel = /m/a-colon.gguf\n\n[a-1]\nmodel = /m/a-dash.gguf\n\n[b:2]\nmodel = /m/b.gguf\n")
	_, body = get(t, s, "/models?reload=1", "k")
	if ids := idsOf(t, body); len(ids) != 1 || ids[0] != "a-1" {
		t.Fatalf("a colon section that arrives with a reload must stay unlisted, got %v", ids)
	}
}

func writePresetFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The listing order is part of what tests read, so it must not depend on map iteration order.
func TestFakeRouterListsPresetModelsInIdOrder(t *testing.T) {
	var presets strings.Builder
	for _, id := range []string{"h", "g", "f", "e", "d", "c", "b", "a"} {
		presets.WriteString("[" + id + "]\nmodel = /m/" + id + ".gguf\n\n")
	}
	path := filepath.Join(t.TempDir(), "presets.ini")
	writePresetFile(t, path, presets.String())
	s := New(t, Options{APIKey: "k", PresetPath: path})
	_, body := get(t, s, "/models", "k")
	if got := strings.Join(idsOf(t, body), " "); got != "a b c d e f g h" {
		t.Fatalf("models must be listed in id order, got %q", got)
	}
}

// A reload can drop a model while its load is still pending; the pending load must not crash the process.
func TestFakeRouterSurvivesAModelRemovedWhileItLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.ini")
	writePresetFile(t, path, "[a]\nmodel = /m/a.gguf\n")
	s := New(t, Options{APIKey: "k", PresetPath: path, LoadDelay: 50 * time.Millisecond})
	if code, body := post(t, s, "/models/load", `{"model":"a"}`); code != http.StatusOK {
		t.Fatalf("load: %d %s", code, body)
	}
	writePresetFile(t, path, "[b]\nmodel = /m/b.gguf\n")
	_, body := get(t, s, "/models?reload=1", "k")
	if ids := idsOf(t, body); len(ids) != 1 || ids[0] != "b" {
		t.Fatalf("reload must drop a: %v", ids)
	}
	time.Sleep(150 * time.Millisecond)
	if st := s.Status("a"); st != "" {
		t.Fatalf("a removed model must stay gone, got %q", st)
	}
}

func waitStatus(t *testing.T, s *Server, id, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.Status(id) != want {
		if time.Now().After(deadline) {
			t.Fatalf("model %s never reached %s, it is %q", id, want, s.Status(id))
		}
		time.Sleep(time.Millisecond)
	}
}

func streamLines(t *testing.T, s *Server, body string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.URL()+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			lines = append(lines, sc.Text())
		}
	}
	return lines
}

func TestFakeRouterRecordsOnlyAuthenticatedRequests(t *testing.T) {
	s := New(t, Options{APIKey: "k"})
	s.AddModel("a", "/m/a.gguf")
	wantDenied := `{"error":{"code":401,"message":"Invalid API Key","type":"authentication_error"}}`
	if code, body := get(t, s, "/models", "wrong"); code != http.StatusUnauthorized || body != wantDenied {
		t.Fatalf("wrong key: %d %s", code, body)
	}
	_, models := get(t, s, "/models?reload=1", "k")
	_, v1 := get(t, s, "/v1/models", "k")
	if models != v1 || len(idsOf(t, models)) != 1 {
		t.Fatalf("/v1/models must mirror /models (a reload without a preset file is a no-op):\n%s\n%s", models, v1)
	}
	got := s.Requests()
	if want := []string{"GET /models?reload=1", "GET /v1/models"}; !slices.Equal(got, want) {
		t.Fatalf("requests: got %v, want %v", got, want)
	}
	got[0] = "tampered"
	if s.Requests()[0] != "GET /models?reload=1" {
		t.Fatal("Requests must return a copy of the log")
	}
}

func TestFakeRouterCloseStopsTheServer(t *testing.T) {
	s := New(t, Options{APIKey: "k"})
	if code, _ := get(t, s, "/health", "k"); code != http.StatusOK {
		t.Fatalf("health before the close: %d", code)
	}
	s.Close()
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/health", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a closed fake must refuse connections")
	}
}

func TestFakeRouterHangLoadStaysLoading(t *testing.T) {
	s := New(t, Options{APIKey: "k", LoadDelay: 5 * time.Millisecond})
	s.AddModel("a", "/m/a.gguf")
	s.HangLoad("a")
	if code, body := post(t, s, "/models/load", `{"model":"a"}`); code != http.StatusOK {
		t.Fatalf("a hanging load is still accepted: %d %s", code, body)
	}
	time.Sleep(60 * time.Millisecond)
	if st := s.Status("a"); st != "loading" {
		t.Fatalf("a hanging load must stay in loading, got %s", st)
	}
	if code, _ := get(t, s, "/props?model=a", "k"); code != http.StatusBadRequest {
		t.Fatalf("props of a model that is still loading must be 400, got %d", code)
	}
}

func TestFakeRouterUnknownModel(t *testing.T) {
	s := New(t, Options{APIKey: "k"})
	if code, body := post(t, s, "/models/unload", `{"model":"nope"}`); code != http.StatusBadRequest || !strings.Contains(body, "not found") {
		t.Fatalf("unload of an unknown model: %d %s", code, body)
	}
	if code, _ := get(t, s, "/props?model=nope", "k"); code != http.StatusBadRequest {
		t.Fatalf("props of an unknown model must be 400, got %d", code)
	}
	s.SetStatus("nope", "loaded")
	if st := s.Status("nope"); st != "" {
		t.Fatalf("SetStatus must not invent a model, got %q", st)
	}
}

func TestFakeRouterToleratesAMissingPresetFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.ini")
	s := New(t, Options{APIKey: "k", PresetPath: path})
	_, body := get(t, s, "/models", "k")
	if ids := idsOf(t, body); len(ids) != 0 {
		t.Fatalf("no preset file, no models: %v", ids)
	}
	writePresetFile(t, path, "[one]\nmodel = /m/one.gguf\n")
	_, body = get(t, s, "/models?reload=1", "k")
	if ids := idsOf(t, body); len(ids) != 1 || ids[0] != "one" {
		t.Fatalf("a preset file that appears must be read: %v", ids)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, s, "/models?reload=1", "k")
	if ids := idsOf(t, body); len(ids) != 1 || ids[0] != "one" {
		t.Fatalf("an unreadable preset file must leave the list as it was: %v", ids)
	}
}

func TestFakeRouterPropsReportTheContextOfThePreset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.ini")
	writePresetFile(t, path, "[short]\nmodel = /m/s.gguf\nc = 2048\n\n[long]\nmodel = /m/l.gguf\nctx-size = 8192\n\n"+
		"[plain]\nmodel = /m/p.gguf\n")
	s := New(t, Options{APIKey: "k", PresetPath: path})
	for _, tc := range []struct {
		id, path string
		ctx      int
	}{{"short", "/m/s.gguf", 2048}, {"long", "/m/l.gguf", 8192}, {"plain", "/m/p.gguf", 4096}} {
		s.SetStatus(tc.id, "loaded")
		code, body := get(t, s, "/props?model="+tc.id, "k")
		var props struct {
			Settings struct {
				NCtx int `json:"n_ctx"`
			} `json:"default_generation_settings"`
			ModelPath string `json:"model_path"`
		}
		if err := json.Unmarshal([]byte(body), &props); err != nil || code != http.StatusOK {
			t.Fatalf("props of %s: %d %s (%v)", tc.id, code, body, err)
		}
		if props.Settings.NCtx != tc.ctx || props.ModelPath != tc.path {
			t.Fatalf("props of %s: n_ctx %d and path %q, want %d and %q", tc.id, props.Settings.NCtx, props.ModelPath, tc.ctx, tc.path)
		}
	}
	s.SetStatus("plain", "sleeping")
	if code, body := get(t, s, "/props?model=plain", "k"); code != http.StatusOK || !strings.Contains(body, `"is_sleeping":true`) {
		t.Fatalf("props of a sleeping model: %d %s", code, body)
	}
}

func TestFakeRouterEvictsTheOldestResidentModel(t *testing.T) {
	s := New(t, Options{APIKey: "k", ModelsMax: 2, LoadDelay: 5 * time.Millisecond})
	ids := []string{"a", "b", "c"}
	for _, id := range ids {
		s.AddModel(id, "/m/"+id+".gguf")
	}
	for _, id := range ids {
		if code, body := post(t, s, "/models/load", `{"model":"`+id+`"}`); code != http.StatusOK {
			t.Fatalf("load %s: %d %s", id, code, body)
		}
		waitStatus(t, s, id, "loaded")
	}
	if a, b, c := s.Status("a"), s.Status("b"), s.Status("c"); a != "unloaded" || b != "loaded" || c != "loaded" {
		t.Fatalf("models-max 2 must evict only the oldest, got a=%s b=%s c=%s", a, b, c)
	}
}

func TestFakeRouterStreamsToolCallsWithoutThinking(t *testing.T) {
	s := New(t, Options{APIKey: "k"})
	s.AddModel("a", "/m/a.gguf")
	lines := streamLines(t, s, `{"model":"a","stream":true,"chat_template_kwargs":{"enable_thinking":false},`+
		`"messages":[],"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, `"tool_calls"`) || !strings.Contains(all, "get_weather") {
		t.Fatalf("a streamed answer to a request with tools must call the tool: %v", lines)
	}
	if strings.Contains(all, "reasoning_content") || strings.Contains(all, `"content":"Hello"`) {
		t.Fatalf("thinking off and a tool call must leave out the reasoning and the plain text: %v", lines)
	}
	if len(lines) == 0 || lines[len(lines)-1] != "data: [DONE]" {
		t.Fatalf("the stream must end with [DONE]: %v", lines)
	}
}

// Later tests drive the fake from several goroutines at once; run under -race this proves its locking.
func TestFakeRouterIsSafeForConcurrentUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.ini")
	writePresetFile(t, path, "[a]\nmodel = /m/a.gguf\n\n[b]\nmodel = /m/b.gguf\n")
	s := New(t, Options{APIKey: "k", PresetPath: path, ModelsMax: 1, LoadDelay: time.Millisecond})
	call := func(method, target, body string) {
		req, err := http.NewRequest(method, s.URL()+target, strings.NewReader(body))
		if err != nil {
			t.Error(err)
			return
		}
		req.Header.Set("Authorization", "Bearer k")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	workers := []string{"a", "b", "a", "b"}
	const rounds, callsPerRound = 10, 5
	var wg sync.WaitGroup
	for _, id := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				call(http.MethodPost, "/models/load", `{"model":"`+id+`"}`)
				call(http.MethodGet, "/models?reload=1", "")
				call(http.MethodGet, "/props?model="+id, "")
				call(http.MethodPost, "/v1/chat/completions", `{"model":"`+id+`","stream":true,"messages":[]}`)
				call(http.MethodPost, "/models/unload", `{"model":"`+id+`"}`)
				s.SetStatus(id, "sleeping")
				_ = s.Status(id)
				_ = s.Requests()
			}
		}()
	}
	wg.Wait()
	if n, want := len(s.Requests()), len(workers)*rounds*callsPerRound; n != want {
		t.Fatalf("every request must be logged exactly once: got %d, want %d", n, want)
	}
}
