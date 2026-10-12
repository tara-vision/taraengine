package llamatest

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestALoadOfARunningModelIsRefused(t *testing.T) {
	s := New(t, Options{APIKey: "k"})
	s.AddModel("a", "/m/a.gguf")
	for _, status := range []string{"loading", "loaded", "sleeping"} {
		s.SetStatus("a", status)
		code, body := post(t, s, "/models/load", `{"model":"a"}`)
		if code != http.StatusBadRequest || !strings.Contains(body, "model is already running") {
			t.Errorf("a load of a model that is %s: %d %s", status, code, body)
		}
		if got := s.Status("a"); got != status {
			t.Errorf("a refused load left the model %s, want %s", got, status)
		}
	}
	s.SetStatus("a", "unloaded")
	if code, body := post(t, s, "/models/load", `{"model":"a"}`); code != http.StatusOK {
		t.Fatalf("a model that is not running loads: %d %s", code, body)
	}
}

func TestAReloadStopsTheRunningModelsWhoseSectionChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.ini")
	writePresetFile(t, path, "[a]\nmodel = /m/a.gguf\nc = 4096\n\n[b]\nmodel = /m/b.gguf\n\n"+
		"[c]\nmodel = /m/c.gguf\n\n[d]\nmodel = /m/d.gguf\n")
	s := New(t, Options{APIKey: "k", PresetPath: path})
	s.SetStatus("a", "loaded")
	s.SetStatus("b", "sleeping")
	s.SetStatus("c", "loading")
	s.SetStatus("d", "loaded")
	writePresetFile(t, path, "[a]\nmodel = /m/a.gguf\nc = 8192\n\n[b]\nmodel = /m/b.gguf\nc = 2048\n\n"+
		"[c]\nmodel = /m/c.gguf\nc = 2048\n\n[d]\nmodel = /m/d.gguf\n")
	if code, body := get(t, s, "/models?reload=1", "k"); code != http.StatusOK {
		t.Fatalf("reload: %d %s", code, body)
	}
	for id, want := range map[string]string{"a": "unloaded", "b": "unloaded", "c": "unloaded", "d": "loaded"} {
		if got := s.Status(id); got != want {
			t.Errorf("%s is %s after the reload, want %s", id, got, want)
		}
	}
}

func TestAnUnloadStopsAModelThatIsStillLoading(t *testing.T) {
	s := New(t, Options{APIKey: "k", LoadDelay: 50 * time.Millisecond, UnloadDelay: time.Minute})
	s.AddModel("a", "/m/a.gguf")
	if code, body := post(t, s, "/models/load", `{"model":"a"}`); code != http.StatusOK {
		t.Fatalf("load: %d %s", code, body)
	}
	if code, body := post(t, s, "/models/unload", `{"model":"a"}`); code != http.StatusOK || body != `{"success":true}` {
		t.Fatalf("an unload of a model that is loading: %d %s", code, body)
	}
	if got := s.Status("a"); got != "unloaded" {
		t.Fatalf("a loading model stops at once, whatever the unload delay: %s", got)
	}
	time.Sleep(150 * time.Millisecond)
	if got := s.Status("a"); got != "unloaded" {
		t.Fatalf("the load that was stopped never comes up: %s", got)
	}
}

func TestSetFailedListsTheModelAsAFailedLoad(t *testing.T) {
	s := New(t, Options{APIKey: "k"})
	s.AddModel("a", "/m/a.gguf")
	s.SetStatus("a", "loading")
	s.SetFailed("a", 137)
	_, body := get(t, s, "/models", "k")
	st := statusOf(t, body, "a")
	if st["value"] != "unloaded" || st["failed"] != true || st["exit_code"].(float64) != 137 {
		t.Fatalf("a load that failed: %v", st)
	}
	s.SetFailed("nope", 1)
	if got := s.Status("nope"); got != "" {
		t.Fatalf("SetFailed must not invent a model, got %q", got)
	}
}
