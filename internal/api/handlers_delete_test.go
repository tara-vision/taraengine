package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/pkg/api"
)

func TestDeleteModel(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusLoaded})
	if resp, _ := h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if len(h.fake.Unloads) != 1 || h.fake.Refreshes != 1 {
		t.Fatalf("a loaded model must be unloaded and the backend refreshed: %+v %d", h.fake.Unloads, h.fake.Refreshes)
	}
	e, _ := h.cat.Registry.Find("gemma4:12b")
	if _, err := os.Stat(h.cat.Store.Path(e.HFRepo, e.File)); !os.IsNotExist(err) {
		t.Fatal("blob still there")
	}
	if resp, _ := h.do(t, http.MethodDelete, "/api/models/qwen3.6:35b", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleting a model that is not pulled: %d", resp.StatusCode)
	}
}

func TestDeleteRemovesEverySiblingAndOnlyUnloadsWhatIsResident(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	e, _ := h.cat.Registry.Find("gemma4:12b")
	for _, status := range []backend.Status{backend.StatusPulled, backend.StatusFailed, backend.StatusFaulty} {
		h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: status})
		pulled(t, h, "gemma4:12b")
		if resp, _ := h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "", nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete while %s: %d", status, resp.StatusCode)
		}
	}
	if len(h.fake.Unloads) != 0 {
		t.Fatalf("a model that is not resident needs no unload: %v", h.fake.Unloads)
	}
	for _, f := range e.Files() {
		if h.cat.Store.Exists(e.HFRepo, f.File) {
			t.Errorf("%s survived the delete", f.File)
		}
	}
	if _, err := os.Stat(filepath.Join(h.cat.Store.Dir, e.HFRepo)); !os.IsNotExist(err) {
		t.Fatalf("the emptied repository directory must be pruned: %v", err)
	}
	for _, status := range []backend.Status{backend.StatusLoading, backend.StatusSleeping, backend.StatusLoaded} {
		pulled(t, h, "gemma4:12b")
		h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: status})
		if resp, _ := h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "", nil); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("delete while %s: %d", status, resp.StatusCode)
		}
	}
	if len(h.fake.Unloads) != 3 {
		t.Fatalf("a loading, sleeping or loaded model must be unloaded first: %v", h.fake.Unloads)
	}
}

func TestDeleteTakesTheNameBeforeItReadsTheModel(t *testing.T) {
	h := newHarness(t, "")
	e, _ := h.cat.Registry.Find("glm-4.7-flash")
	holdPulls(t, h, e.File)
	first := make(chan int, 1)
	go func() {
		status, _, _, err := h.request(http.MethodPost, "/api/pull?wait=1", "application/json", "", strings.NewReader(`{"model":"glm-4.7-flash"}`))
		if err != nil {
			status = -1
		}
		first <- status
	}()
	waitFor(t, h.fetcher.started) // the pull holds the name, and the model's file is not in place yet
	resp, data := h.do(t, http.MethodDelete, "/api/models/glm-4.7-flash", "", nil)
	if body := decode[api.Error](t, data).Error; resp.StatusCode != http.StatusConflict || body.Message != "a pull of glm-4.7-flash is running" {
		t.Fatalf("a delete must meet the pull that holds the name, not a model that looks unpulled: %d %s", resp.StatusCode, data)
	}
	close(h.fetcher.gate)
	if status := waitFor(t, first); status != 200 {
		t.Fatalf("the pull: %d", status)
	}
}

func TestDeleteHoldsTheNameWhileItReadsTheModel(t *testing.T) {
	gated := newGatedBackend(t, "models")
	h := newHarness(t, "", func(d *Deps) {
		gated.Backend = d.Backend
		d.Backend = gated
	})
	pulled(t, h, "gemma4:12b")
	first := make(chan int, 1)
	go func() {
		status, _, _, err := h.request(http.MethodDelete, "/api/models/gemma4:12b", "", "", nil)
		if err != nil {
			status = -1
		}
		first <- status
	}()
	waitFor(t, gated.started) // the delete is reading the model's status from the backend
	resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "gemma4:12b"})
	if body := decode[api.Error](t, data).Error; resp.StatusCode != http.StatusConflict || body.Message != "a delete of gemma4:12b is running" {
		t.Fatalf("what the delete reads must not change under it, so the name is already its own: %d %s", resp.StatusCode, data)
	}
	close(gated.gate)
	if status := waitFor(t, first); status != http.StatusNoContent {
		t.Fatalf("the delete: %d", status)
	}
}

func TestTheRefusalNamesWhatHoldsTheName(t *testing.T) {
	gated := newGatedBackend(t, "unload")
	h := newHarness(t, "", func(d *Deps) {
		gated.Backend = d.Backend
		d.Backend = gated
	})
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusLoaded})
	first := make(chan int, 1)
	go func() {
		status, _, _, err := h.request(http.MethodDelete, "/api/models/gemma4:12b", "", "", nil)
		if err != nil {
			status = -1
		}
		first <- status
	}()
	waitFor(t, gated.started) // the delete is inside its unload and holds the name
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodDelete, "/api/models/gemma4:12b", ""},
		{http.MethodPost, "/api/pull", `{"model":"gemma4:12b"}`},
		{http.MethodPost, "/api/pull?wait=1", `{"model":"gemma4:12b"}`},
	} {
		status, _, data, err := h.request(tc.method, tc.path, "application/json", "", strings.NewReader(tc.body))
		if body := decode[api.Error](t, data).Error; err != nil || status != http.StatusConflict || body.Message != "a delete of gemma4:12b is running" {
			t.Errorf("%s %s while a delete runs: %d %s %v", tc.method, tc.path, status, data, err)
		}
	}
	close(gated.gate)
	if status := waitFor(t, first); status != http.StatusNoContent {
		t.Fatalf("the delete: %d", status)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "gemma4:12b"}); resp.StatusCode != 200 {
		t.Fatalf("the name is free after the delete: %d", resp.StatusCode)
	}
}

func TestDeleteRefusesBadAndUnknownNames(t *testing.T) {
	h := newHarness(t, "")
	for _, name := range []string{"..%2Fetc", "a%2Fb", "-x"} {
		if resp, _ := h.do(t, http.MethodDelete, "/api/models/"+name, "", nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: %d", name, resp.StatusCode)
		}
	}
	resp, data := h.do(t, http.MethodDelete, "/api/models/absent", "", nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(decode[api.Error](t, data).Error.Message, "absent") {
		t.Fatalf("absent: %d %s", resp.StatusCode, data)
	}
	resp, data = h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "", nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(decode[api.Error](t, data).Error.Message, "not pulled") {
		t.Fatalf("not pulled: %d %s", resp.StatusCode, data)
	}
	if h.fake.Refreshes != 0 {
		t.Fatalf("nothing was deleted, so nothing is refreshed: %d", h.fake.Refreshes)
	}
}

func TestDeleteStopsWhenTheBackendCannotUnload(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusLoaded})
	h.fake.Fail("unload", backend.ErrNotRunning)
	if resp, _ := h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	e, _ := h.cat.Registry.Find("gemma4:12b")
	if !h.cat.Store.Exists(e.HFRepo, e.File) || h.fake.Refreshes != 0 {
		t.Fatal("a model that could not be unloaded must keep its files")
	}
}

func TestDeleteReportsABackendThatCannotRefresh(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	h.fake.Fail("refresh", backend.ErrNotRunning)
	if resp, _ := h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "", nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	e, _ := h.cat.Registry.Find("gemma4:12b")
	if h.cat.Store.Exists(e.HFRepo, e.File) {
		t.Fatal("the files are deleted before the backend is told")
	}
}

func TestDeleteReportsAStoreThatCannotRemove(t *testing.T) {
	h := newHarness(t, "")
	pulled(t, h, "gemma4:12b")
	e, _ := h.cat.Registry.Find("gemma4:12b")
	stuck := h.cat.Store.Path(e.HFRepo, e.MMProj.File)
	if err := os.Remove(stuck); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stuck, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stuck, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, data := h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "", nil)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(decode[api.Error](t, data).Error.Message, "remove") {
		t.Fatalf("delete: %d %s", resp.StatusCode, data)
	}
	if h.fake.Refreshes != 0 {
		t.Fatal("a failed delete must not refresh the backend")
	}
	if logs := h.logs.String(); !strings.Contains(logs, "delete failed") || !strings.Contains(logs, "gemma4:12b") {
		t.Fatalf("the operator must find the failure in the log: %s", logs)
	}
}

func TestDeleteNeverTouchesAFileOutsideTheStore(t *testing.T) {
	h := newHarness(t, "")
	outside := filepath.Join(t.TempDir(), "precious.gguf")
	if err := os.WriteFile(outside, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := h.cat.Store.Dir + "-extra"
	if err := os.MkdirAll(sibling, 0o750); err != nil {
		t.Fatal(err)
	}
	lookalike := filepath.Join(sibling, "lookalike.gguf")
	if err := os.WriteFile(lookalike, []byte("keep me too"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"evil": outside, "lookalike": lookalike, "root": h.cat.Store.Dir} {
		if err := h.cat.Index.Add(catalog.IndexEntry{Name: name, Path: path, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"evil", "lookalike", "root"} {
		resp, data := h.do(t, http.MethodDelete, "/api/models/"+name, "", nil)
		if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(decode[api.Error](t, data).Error.Message, "leave the store") {
			t.Errorf("%s: %d %s", name, resp.StatusCode, data)
		}
	}
	for _, p := range []string{outside, lookalike} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s must survive a delete through a hand-edited index: %v", p, err)
		}
	}
	entries, err := h.cat.Index.Load()
	if err != nil || len(entries) != 3 {
		t.Fatalf("a refused delete keeps the index entries: %+v %v", entries, err)
	}
	if h.fake.Refreshes != 0 {
		t.Fatal("nothing was deleted, so nothing is refreshed")
	}
}

func TestRemoveRefusesAPathThatIsNotUnderTheStoreRoot(t *testing.T) {
	h := newHarness(t, "")
	s := newServer(h.deps)
	err := s.remove(catalog.Item{Name: "relative", ModelPath: filepath.Join("relative", "x.gguf")})
	if err == nil || !strings.Contains(err.Error(), "leave the store") {
		t.Fatalf("a path with no relation to the store root must be refused: %v", err)
	}
}

func TestDeleteDropsAnIndexedModelWhoseFileIsGone(t *testing.T) {
	h := newHarness(t, "")
	path := addHFModel(t, h, "extra", "org/repo", "extra.gguf")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if m := modelsByName(t, h)["extra"]; m.Status != "available" {
		t.Fatalf("a model whose file is gone shows as a row that was never pulled: %+v", m)
	}
	if resp, data := h.do(t, http.MethodDelete, "/api/models/extra", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, data)
	}
	if entries, err := h.cat.Index.Load(); err != nil || len(entries) != 0 {
		t.Fatalf("the entry must go with the delete: %+v %v", entries, err)
	}
	if _, listed := modelsByName(t, h)["extra"]; listed {
		t.Fatal("the row must be gone")
	}
	if h.fake.Refreshes != 1 || len(h.fake.Unloads) != 0 {
		t.Fatalf("the backend is told, and nothing was running: %d %v", h.fake.Refreshes, h.fake.Unloads)
	}
	if resp, _ := h.do(t, http.MethodDelete, "/api/models/extra", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a second delete finds nothing: %d", resp.StatusCode)
	}
}

func TestDeleteDropsALocalModelWhoseOriginalMoved(t *testing.T) {
	h := newHarness(t, "")
	local := filepath.Join(t.TempDir(), "Mine.gguf")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: local}); resp.StatusCode != 200 {
		t.Fatalf("pull: %d", resp.StatusCode)
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	link := h.cat.Store.Path("local", "Mine.gguf")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the link dangles but is there: %v", err)
	}
	if resp, data := h.do(t, http.MethodDelete, "/api/models/Mine", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d %s", resp.StatusCode, data)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the dangling link must be removed: %v", err)
	}
	if entries, _ := h.cat.Index.Load(); len(entries) != 0 {
		t.Fatalf("index: %+v", entries)
	}
}

func TestDeleteStillRefusesACatalogModelThatIsNotPulled(t *testing.T) {
	h := newHarness(t, "")
	addHFModel(t, h, "extra", "org/repo", "extra.gguf")
	resp, data := h.do(t, http.MethodDelete, "/api/models/qwen3.6:35b", "", nil)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(decode[api.Error](t, data).Error.Message, "not pulled") {
		t.Fatalf("a catalog row that was never pulled has nothing to remove: %d %s", resp.StatusCode, data)
	}
	if entries, _ := h.cat.Index.Load(); len(entries) != 1 {
		t.Fatalf("another model's entry stays: %+v", entries)
	}
}
