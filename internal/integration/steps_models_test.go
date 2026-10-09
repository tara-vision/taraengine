//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"slices"
	"testing"

	pinned "github.com/tara-vision/taraengine/internal/runtime"
	"github.com/tara-vision/taraengine/pkg/api"
)

// checkRuntime asserts that the runtime the daemon was given is the pinned build, installed from the tarball the lock
// pins and intact on disk, and that the daemon runs that build.
func checkRuntime(t *testing.T, s *scenario) {
	lock, err := pinned.LoadLock()
	if err != nil {
		t.Fatalf("the embedded lock: %v", err)
	}
	manifest, err := pinned.Verify(s.env.runtimeDir)
	if err != nil {
		t.Fatalf("the installed runtime does not verify: %v", err)
	}
	if manifest.Build != lock.Build {
		t.Fatalf("installed build %s, the lock pins %s", manifest.Build, lock.Build)
	}
	if asset, ok := lock.Assets[manifest.Key]; !ok || manifest.TarballSHA256 != asset.SHA256 {
		t.Fatalf("the install of %s came from the tarball %s, which the lock does not pin",
			manifest.Key, manifest.TarballSHA256)
	}
	v := s.version(t)
	if v.Runtime.Build != lock.Build || v.Runtime.Backend != "llama.cpp" || v.Platform != manifest.Key {
		t.Fatalf("the daemon reports %+v, want build %s on %s", v, lock.Build, manifest.Key)
	}
	if h, err := s.c.Health(within(t, callTimeout)); err != nil || h.Status != "ok" || h.Backend != "llama.cpp" {
		t.Fatalf("health: %+v %v", h, err)
	}
}

// checkCatalog asserts that the catalog of the data directory is what the daemon serves, with nothing pulled or loaded.
func checkCatalog(t *testing.T, s *scenario) {
	models, err := s.c.Models(within(t, callTimeout))
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	want := api.Model{
		Name: s.entry.Name, Status: "available", Size: s.entry.Size, Family: "tinyllamas", Params: "260K",
		Quantization: "F32", Context: contextWindow, NativeContext: contextWindow, Capabilities: []string{},
		Tier: "small", Default: true,
	}
	if !reflect.DeepEqual(models, []api.Model{want}) {
		t.Fatalf("catalog: got %+v, want [%+v]", models, want)
	}
	if loaded, err := s.c.Loaded(within(t, callTimeout)); err != nil || len(loaded) != 0 {
		t.Fatalf("loaded before any load: %+v %v", loaded, err)
	}
}

// checkPull pulls the test model from the real hub and asserts the events, the file in the store and the status.
func checkPull(t *testing.T, s *scenario) {
	var events []api.PullEvent
	err := s.c.Pull(within(t, pullTimeout), s.entry.Name, func(e api.PullEvent) { events = append(events, e) })
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("the pull reported no events")
	}
	if last := events[len(events)-1]; last.Stage != "done" || last.File != "" {
		t.Fatalf("the pull did not end with the overall done event: %+v", events)
	}
	for _, want := range []api.PullEvent{
		{Stage: "resolving", File: s.entry.File, Total: s.entry.Size},
		{Stage: "downloading", File: s.entry.File, Bytes: s.entry.Size, Total: s.entry.Size},
		{Stage: "verifying", File: s.entry.File, Bytes: s.entry.Size, Total: s.entry.Size},
		{Stage: "done", File: s.entry.File, Bytes: s.entry.Size, Total: s.entry.Size},
	} {
		if !slices.Contains(events, want) {
			t.Fatalf("the pull never reported %+v: %+v", want, events)
		}
	}
	data, err := os.ReadFile(s.modelPath())
	if err != nil {
		t.Fatalf("the pulled file: %v", err)
	}
	if sum := sha256.Sum256(data); int64(len(data)) != s.entry.Size || hex.EncodeToString(sum[:]) != s.entry.SHA256 {
		t.Fatalf("the pulled file is %d bytes with sha256 %x, want %d bytes with %s",
			len(data), sum, s.entry.Size, s.entry.SHA256)
	}
	if m := s.model(t); m.Status != "pulled" {
		t.Fatalf("after the pull the status is %q: %+v", m.Status, m)
	}
}

// checkLoad loads the model and asserts that it reaches loaded in the window of its preset, in the API and in the
// router's own list, which is also the list a client of /v1 sees.
func checkLoad(t *testing.T, s *scenario) {
	m, err := s.c.Load(within(t, loadTimeout), s.entry.Name, 0, false)
	if err != nil || m.Status != "loaded" || m.Context != contextWindow {
		t.Fatalf("load: %+v %v", m, err)
	}
	assertListedAt(t, s, contextWindow)
}

// assertListedAt asserts that /v1/models lists exactly the scenario's model, under its catalog name, loaded with the
// window the router really runs it at.
func assertListedAt(t *testing.T, s *scenario, window int) {
	t.Helper()
	listed := s.listed(t)
	if len(listed) != 1 || listed[0].ID != s.entry.Name || listed[0].Status.Value != "loaded" ||
		listed[0].Meta.NCtx != window {
		t.Fatalf("/v1/models lists %+v, want %s loaded at %d tokens", listed, s.entry.Name, window)
	}
}

// checkPs asserts that the loaded list shows the model with a memory figure and says how the figure was found.
func checkPs(t *testing.T, s *scenario) {
	loaded, err := s.c.Loaded(within(t, callTimeout))
	if err != nil || len(loaded) != 1 {
		t.Fatalf("loaded: %+v %v", loaded, err)
	}
	got := loaded[0]
	if got.Name != s.entry.Name || got.Context != contextWindow || got.Memory == nil || got.Memory.Bytes <= 0 {
		t.Fatalf("loaded model: %+v (memory %+v)", got, got.Memory)
	}
	if src := got.Memory.Source; src != "measured" && src != "estimated" {
		t.Fatalf("the memory figure says it was %q, want measured or estimated", src)
	}
}

// checkUnload unloads the model and asserts that it is gone by the time the call returns. The backend waits for the
// router to stop the model before it answers, so a load that follows cannot be answered by the old instance, and
// there is nothing to poll here. The model is back to pulled, and holds no memory.
func checkUnload(t *testing.T, s *scenario) {
	if err := s.c.Unload(within(t, callTimeout), s.entry.Name); err != nil {
		t.Fatalf("unload: %v", err)
	}
	if m := s.model(t); m.Status != "pulled" || m.Memory != nil {
		t.Fatalf("after the unload: %+v", m)
	}
}

// checkRemove removes the pulled model and asserts that its file is gone and the catalog offers it again.
func checkRemove(t *testing.T, s *scenario) {
	if err := s.c.Delete(within(t, callTimeout), s.entry.Name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(s.modelPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the model file is still there after the delete: %v", err)
	}
	if m := s.model(t); m.Status != "available" {
		t.Fatalf("after the delete: %+v", m)
	}
}
