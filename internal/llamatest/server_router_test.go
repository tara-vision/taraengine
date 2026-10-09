package llamatest

import (
	"net/http"
	"testing"
	"time"
)

// awaitStatus polls until the model has the status, for at most five seconds.
func awaitStatus(t *testing.T, s *Server, id, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for s.Status(id) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s, want %s", id, s.Status(id), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUnloadLeavesTheModelLoadedForTheUnloadDelay(t *testing.T) {
	const delay = time.Second
	s := New(t, Options{APIKey: "k", UnloadDelay: delay})
	s.AddModel("a", "/m/a.gguf")
	s.AddModel("b", "/m/b.gguf")
	s.SetStatus("a", "loaded")
	s.SetStatus("b", "loaded")
	// b is unloaded first and loads again within its delay; its late unload must leave it loading.
	post(t, s, "/models/unload", `{"model":"b"}`)
	s.SetStatus("b", "loading")
	began := time.Now()
	if code, body := post(t, s, "/models/unload", `{"model":"a"}`); code != http.StatusOK || body != `{"success":true}` {
		t.Fatalf("unload: %d %s", code, body)
	}
	if st := s.Status("a"); st != "loaded" && time.Since(began) < delay/2 {
		t.Fatalf("well inside the delay the router still lists the model as loaded, got %s", st)
	}
	awaitStatus(t, s, "a", "unloaded")
	if st := s.Status("b"); st != "loading" {
		t.Fatalf("the late unload touched a model that is loading again: %s", st)
	}
}

func TestRouterAnswersCORSForEveryOrigin(t *testing.T) {
	s := New(t, Options{APIKey: "k"})
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/health", nil)
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://evil.example" {
		t.Fatalf("the router reflects any origin, got %q", got)
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("headers %v", resp.Header)
	}
	preflight, _ := http.NewRequest(http.MethodOptions, s.URL()+"/v1/chat/completions", nil)
	preflight.Header.Set("Origin", "https://evil.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err = http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "https://evil.example" ||
		resp.Header.Get("Access-Control-Allow-Methods") == "" || resp.Header.Get("Access-Control-Allow-Headers") == "" {
		t.Fatalf("a preflight without a key is approved: %d %v", resp.StatusCode, resp.Header)
	}
	if n := len(s.Requests()); n != 1 {
		t.Fatalf("a preflight is answered before the key check and not recorded, requests %v", s.Requests())
	}
	plain, err := http.Get(s.URL() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = plain.Body.Close()
	if got := plain.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("no origin, no CORS answer: %q", got)
	}
}
