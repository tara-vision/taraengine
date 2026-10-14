package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/pkg/api"
)

func pullEvents(t *testing.T, h *harness, ref string) (int, []api.PullEvent) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/pull", strings.NewReader(`{"model":"`+ref+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var events []api.PullEvent
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e api.PullEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatalf("event %q: %v", line, err)
		}
		events = append(events, e)
	}
	return resp.StatusCode, events
}

// holdPulls makes the fetch of file wait until the test opens the gate; the gate opens at the end of the test
// whatever happens, so a failing test cannot leave a handler stuck.
func holdPulls(t *testing.T, h *harness, file string) {
	t.Helper()
	gate := make(chan struct{})
	h.fetcher.gateFile, h.fetcher.gate = file, gate
	h.fetcher.started, h.fetcher.stopped = make(chan struct{}, 4), make(chan error, 4)
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
}

func waitFor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the server")
		panic("unreachable")
	}
}

// eventually retries ok until it holds; the server frees a name a moment after its last answer.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never became true: %s", what)
}

func TestPullRegistryStreamsEventsAndRefreshes(t *testing.T) {
	h := newHarness(t, "")
	code, events := pullEvents(t, h, "gemma4:12b")
	if code != 200 || len(events) == 0 {
		t.Fatalf("code %d, events %v", code, events)
	}
	var files []string
	for _, e := range events {
		if e.Stage == "done" && e.File != "" {
			files = append(files, e.File)
		}
		if e.Error != "" {
			t.Fatalf("error event: %+v", e)
		}
	}
	if strings.Join(files, ",") != "gemma-4-12B-it-Q4_0.gguf,mmproj-gemma-4-12B-it-Q8_0.gguf,mtp-gemma-4-12B-it-Q4_0.gguf" {
		t.Fatalf("files done: %v", files)
	}
	if last := events[len(events)-1]; last.Stage != "done" || last.File != "" {
		t.Fatalf("last event must be the overall done: %+v", last)
	}
	e, _ := h.cat.Registry.Find("gemma4:12b")
	if !h.cat.Store.Exists(e.HFRepo, e.MTP.File) {
		t.Fatal("the draft sibling was not pulled")
	}
	if h.fake.Refreshes != 1 {
		t.Fatalf("refreshes %d", h.fake.Refreshes)
	}
	code, events = pullEvents(t, h, "gemma4:12b")
	if code != 200 || len(events) != 4 {
		t.Fatalf("a second pull of a pulled model must answer done per file and once overall: %d %v", code, events)
	}
	for _, e := range events {
		if e.Stage != "done" {
			t.Fatalf("a second pull must not download again: %+v", e)
		}
	}
}

func TestPullReportsEachStageOfEachFile(t *testing.T) {
	h := newHarness(t, "")
	_, events := pullEvents(t, h, "glm-4.7-flash")
	e, _ := h.cat.Registry.Find("glm-4.7-flash")
	want := []api.PullEvent{
		{Stage: "resolving", File: e.File, Total: e.Size},
		{Stage: "downloading", File: e.File, Bytes: 32, Total: 64},
		{Stage: "verifying", File: e.File, Bytes: 64, Total: 64},
		{Stage: "done", File: e.File, Bytes: 64, Total: 64},
		{Stage: "done"},
	}
	if len(events) != len(want) {
		t.Fatalf("events: %+v", events)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("event %d: %+v, want %+v", i, events[i], want[i])
		}
	}
}

func TestPullSkipsOnlyTheFilesThatAreInPlace(t *testing.T) {
	h := newHarness(t, "")
	e, _ := h.cat.Registry.Find("gemma4:12b")
	main := h.cat.Store.Path(e.HFRepo, e.File)
	if err := os.MkdirAll(filepath.Dir(main), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, events := pullEvents(t, h, "gemma4:12b")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	if first := events[0]; first.Stage != "done" || first.File != e.File || first.Bytes != e.Size || first.Total != e.Size {
		t.Fatalf("the file in place is reported done at once: %+v", first)
	}
	resolving := 0
	for _, ev := range events {
		if ev.Stage == "resolving" {
			resolving++
			if ev.File == e.File {
				t.Fatalf("the file in place was fetched again: %+v", ev)
			}
		}
	}
	if resolving != 2 {
		t.Fatalf("the two siblings are fetched: %d resolving events in %+v", resolving, events)
	}
	if b, _ := os.ReadFile(main); string(b) != "x" {
		t.Fatalf("the file in place was overwritten: %q", b)
	}
}

func TestPullWaitAndErrors(t *testing.T) {
	h := newHarness(t, "")
	resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "glm-4.7-flash"})
	if resp.StatusCode != 200 || !strings.Contains(string(data), `"done"`) {
		t.Fatalf("wait: %d %s", resp.StatusCode, data)
	}
	if got := decode[map[string]string](t, data); got["status"] != "done" || got["model"] != "glm-4.7-flash" {
		t.Fatalf("wait answer: %v", got)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type %q", got)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "../x"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad ref: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "absent:1"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown catalog name: %d", resp.StatusCode)
	}
	h.fetcher.fail = "Qwen3.6-27B-Q4_K_M.gguf"
	resp, data = h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "qwen3.6:27b"})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(data), "boom") {
		t.Fatalf("fetch failure: %d %s", resp.StatusCode, data)
	}
	_, events := pullEvents(t, h, "qwen3.6:27b")
	if last := events[len(events)-1]; last.Stage != "error" || !strings.Contains(last.Error, "boom") {
		t.Fatalf("streamed failure: %+v", last)
	}
}

func TestOnlyWaitOneWaits(t *testing.T) {
	h := newHarness(t, "")
	for _, query := range []string{"?wait=0", "?wait=true", "?wait="} {
		status, data := h.doRaw(t, http.MethodPost, "/api/pull"+query, "application/json", `{"model":"glm-4.7-flash"}`)
		if status != 200 || !strings.HasPrefix(string(data), "data: ") {
			t.Errorf("%s must stream: %d %q", query, status, data)
		}
	}
}

func TestPullEscapeHatches(t *testing.T) {
	h := newHarness(t, "")
	resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/extra.gguf"})
	if resp.StatusCode != 200 {
		t.Fatalf("hf pull: %d", resp.StatusCode)
	}
	entries, _ := h.cat.Index.Load()
	if len(entries) != 1 || entries[0].Name != "extra" || entries[0].Repo != "org/repo" || entries[0].SHA256 != "abc" {
		t.Fatalf("index: %+v", entries)
	}
	_, data := h.do(t, http.MethodGet, "/api/models", "", nil)
	models := decode[[]api.Model](t, data)
	var extra api.Model
	for _, m := range models {
		if m.Name == "extra" {
			extra = m
		}
	}
	if extra.Status != "pulled" || !extra.CapabilitiesInferred {
		t.Fatalf("extra: %+v", extra)
	}
	local := filepath.Join(t.TempDir(), "Mine.gguf")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: local}); resp.StatusCode != 200 {
		t.Fatalf("local: %d", resp.StatusCode)
	}
	entries, _ = h.cat.Index.Load()
	if len(entries) != 2 || entries[0].Name != "Mine" || entries[0].Repo != "" {
		t.Fatalf("index after local: %+v", entries)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/gemma4:12b.gguf"}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a name the catalog owns must be refused: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodDelete, "/api/models/extra", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete hf model: %d", resp.StatusCode)
	}
	entries, _ = h.cat.Index.Load()
	if len(entries) != 1 {
		t.Fatalf("index after delete: %+v", entries)
	}
	if _, err := os.Stat(h.cat.Store.Path("org/repo", "extra.gguf")); !os.IsNotExist(err) {
		t.Fatal("hf blob still there")
	}
	if _, err := os.Stat(filepath.Join(h.cat.Store.Dir, "org")); !os.IsNotExist(err) {
		t.Fatalf("the emptied directories must be pruned: %v", err)
	}
}

func TestPullRecordsTheHFModelWithItsBlobAndChecksum(t *testing.T) {
	h := newHarness(t, "")
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/extra.gguf"}); resp.StatusCode != 200 {
		t.Fatalf("hf pull: %d", resp.StatusCode)
	}
	entries, _ := h.cat.Index.Load()
	want := h.cat.Store.Path("org/repo", "extra.gguf")
	if len(entries) != 1 || entries[0].File != "extra.gguf" || entries[0].Path != want || entries[0].Size != 32 {
		t.Fatalf("index entry: %+v", entries)
	}
	if info, err := os.Stat(want); err != nil || info.Size() != 32 {
		t.Fatalf("blob: %v %v", info, err)
	}
	if h.fake.Refreshes != 1 {
		t.Fatalf("refreshes %d", h.fake.Refreshes)
	}
}

func TestPullLocalLinksTheFileAndDeleteRemovesOnlyTheLink(t *testing.T) {
	h := newHarness(t, "")
	local := filepath.Join(t.TempDir(), "Mine.gguf")
	if err := os.WriteFile(local, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: local}); resp.StatusCode != 200 {
		t.Fatalf("local: %d", resp.StatusCode)
	}
	link := h.cat.Store.Path("local", "Mine.gguf")
	if target, err := os.Readlink(link); err != nil || target != local {
		t.Fatalf("link: %q %v", target, err)
	}
	entries, _ := h.cat.Index.Load()
	if len(entries) != 1 || entries[0].Path != link || entries[0].Size != int64(len("precious")) {
		t.Fatalf("index: %+v", entries)
	}
	if m := modelsByName(t, h)["Mine"]; m.Status != "pulled" || m.Size != int64(len("precious")) || !m.CapabilitiesInferred {
		t.Fatalf("model: %+v", m)
	}
	if resp, _ := h.do(t, http.MethodDelete, "/api/models/Mine", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("the link survived: %v", err)
	}
	if b, err := os.ReadFile(local); err != nil || string(b) != "precious" {
		t.Fatalf("deleting an import must never touch the original file: %q %v", b, err)
	}
	if entries, _ := h.cat.Index.Load(); len(entries) != 0 {
		t.Fatalf("index after delete: %+v", entries)
	}
	if h.fake.Refreshes != 2 {
		t.Fatalf("refreshes %d", h.fake.Refreshes)
	}
}

func TestPullRefusalsComeBeforeTheStream(t *testing.T) {
	h := newHarness(t, "")
	dir := t.TempDir()
	directory := filepath.Join(dir, "dir.gguf")
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	taken := filepath.Join(dir, "gemma4:12b.gguf")
	if err := os.WriteFile(taken, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ref     string
		status  int
		message string
	}{
		{"absent:1", http.StatusNotFound, "unknown catalog name: absent:1"},
		{filepath.Join(dir, "missing.gguf"), http.StatusNotFound, "missing.gguf is not a readable file"},
		{directory, http.StatusNotFound, "dir.gguf is not a readable file"},
		{taken, http.StatusConflict, "gemma4:12b belongs to the catalog"},
		{"hf://org/repo/gemma4:12b.gguf", http.StatusConflict, "gemma4:12b belongs to the catalog"},
		{"hf://org/repo/x.txt", http.StatusBadRequest, "must name a .gguf file"},
		{"../x", http.StatusBadRequest, "model name"},
		{"", http.StatusBadRequest, "model name is empty"},
	}
	for _, tc := range cases {
		for _, path := range []string{"/api/pull", "/api/pull?wait=1"} {
			resp, data := h.do(t, http.MethodPost, path, "", api.PullRequest{Model: tc.ref})
			body := decode[api.Error](t, data).Error
			if resp.StatusCode != tc.status || !strings.Contains(body.Message, tc.message) || body.Hint == "" {
				t.Errorf("%s as %q: %d %s", path, tc.ref, resp.StatusCode, data)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("%s as %q: a refusal must be JSON, not a stream: %q", path, tc.ref, got)
			}
		}
	}
	if entries, _ := h.cat.Index.Load(); len(entries) != 0 || h.fake.Refreshes != 0 {
		t.Fatalf("a refused pull must change nothing: %+v %d", entries, h.fake.Refreshes)
	}
	if code, events := pullEvents(t, h, "glm-4.7-flash"); code != 200 || events[len(events)-1].Stage != "done" {
		t.Fatalf("refusals must not leave a name claimed: %d %v", code, events)
	}
}

func TestAPullOfTheSameModelIsRefusedWhileOneRuns(t *testing.T) {
	h := newHarness(t, "")
	e, _ := h.cat.Registry.Find("gemma4:12b")
	holdPulls(t, h, e.MTP.File)
	first := make(chan int, 1)
	go func() {
		status, _, _, err := h.request(http.MethodPost, "/api/pull?wait=1", "application/json", "", strings.NewReader(`{"model":"gemma4:12b"}`))
		if err != nil {
			status = -1
		}
		first <- status
	}()
	waitFor(t, h.fetcher.started)

	for _, path := range []string{"/api/pull", "/api/pull?wait=1"} {
		resp, data := h.do(t, http.MethodPost, path, "", api.PullRequest{Model: "gemma4:12b"})
		if body := decode[api.Error](t, data).Error; resp.StatusCode != http.StatusConflict || !strings.Contains(body.Message, "pull of gemma4:12b is running") {
			t.Errorf("%s while a pull runs: %d %s", path, resp.StatusCode, data)
		}
	}
	if resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "glm-4.7-flash"}); resp.StatusCode != 200 {
		t.Fatalf("a pull of another model runs beside it: %d %s", resp.StatusCode, data)
	}
	resp, data := h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "", nil)
	if body := decode[api.Error](t, data).Error; resp.StatusCode != http.StatusConflict || !strings.Contains(body.Message, "pull of gemma4:12b is running") {
		t.Fatalf("a model must not be deleted under its own download: %d %s", resp.StatusCode, data)
	}
	if !h.cat.Store.Exists(e.HFRepo, e.File) {
		t.Fatal("the refused delete removed a file")
	}

	close(h.fetcher.gate)
	if status := waitFor(t, first); status != 200 {
		t.Fatalf("the running pull: %d", status)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "gemma4:12b"}); resp.StatusCode != 200 {
		t.Fatalf("the name is free again: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("and so is the delete: %d", resp.StatusCode)
	}
}

// openStream starts a streaming pull and returns its response once the headers arrive.
func openStream(ctx context.Context, t *testing.T, h *harness, ref string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.srv.URL+"/api/pull", strings.NewReader(`{"model":"`+ref+`"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatalf("the stream must open while the pull is still running: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// readUntil reads events until stop says so, and fails when the stream ends or stalls first.
func readUntil(t *testing.T, sc *bufio.Scanner, stop func(api.PullEvent) bool) []api.PullEvent {
	t.Helper()
	var events []api.PullEvent
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e api.PullEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatalf("event %q: %v", line, err)
		}
		events = append(events, e)
		if stop(e) {
			return events
		}
	}
	t.Fatalf("the stream ended or stalled after %+v: %v", events, sc.Err())
	return nil
}

func TestPullEventsReachTheClientWhileTheDownloadIsStillRunning(t *testing.T) {
	h := newHarness(t, "")
	e, _ := h.cat.Registry.Find("gemma4:12b")
	holdPulls(t, h, e.MTP.File)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp := openStream(ctx, t, h, "gemma4:12b")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	events := readUntil(t, sc, func(ev api.PullEvent) bool { return ev.Stage == "done" && ev.File == e.MMProj.File })
	waitFor(t, h.fetcher.started) // the handler is now inside the last fetch, so what was read was flushed event by event
	if len(events) != 8 || events[0].Stage != "resolving" {
		t.Fatalf("events before the last file: %+v", events)
	}
	close(h.fetcher.gate)
	rest := readUntil(t, sc, func(ev api.PullEvent) bool { return ev.Stage == "done" && ev.File == "" })
	if len(rest) < 4 {
		t.Fatalf("events after the gate opened: %+v", rest)
	}
}

func TestAClientThatLeavesEndsThePullAndFreesTheName(t *testing.T) {
	h := newHarness(t, "")
	e, _ := h.cat.Registry.Find("gemma4:12b")
	holdPulls(t, h, e.MTP.File)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := openStream(ctx, t, h, "gemma4:12b")
	sc := bufio.NewScanner(resp.Body)
	readUntil(t, sc, func(ev api.PullEvent) bool { return ev.Stage == "done" && ev.File == e.MMProj.File })
	waitFor(t, h.fetcher.started)
	cancel()
	if err := waitFor(t, h.fetcher.stopped); !errors.Is(err, context.Canceled) {
		t.Fatalf("the download must be cancelled with the request: %v", err)
	}
	part := h.cat.Store.Path(e.HFRepo, e.MTP.File) + ".part"
	if b, err := os.ReadFile(part); err != nil || string(b) != "half" {
		t.Fatalf("a pull that is cut off keeps what it has downloaded, so that the next one resumes: %q %v", b, err)
	}
	if h.cat.Store.Exists(e.HFRepo, e.MTP.File) {
		t.Fatal("a part file is not the model file")
	}
	close(h.fetcher.gate)
	eventually(t, "the name is free after the client left", func() bool {
		status, _, _, err := h.request(http.MethodPost, "/api/pull?wait=1", "application/json", "", strings.NewReader(`{"model":"gemma4:12b"}`))
		return err == nil && status == 200
	})
	if logs := h.logs.String(); !strings.Contains(logs, "pull failed") || !strings.Contains(logs, "context canceled") {
		t.Fatalf("a cancelled pull is logged: %s", logs)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) || !h.cat.Store.Exists(e.HFRepo, e.MTP.File) {
		t.Fatalf("the pull that resumes promotes the part file: %v", err)
	}
}

func TestPullRefusesAChangedUpstream(t *testing.T) {
	h := newHarness(t, "")
	e, _ := h.cat.Registry.Find("gemma4:12b")
	key := e.HFRepo + "/" + e.File
	art := h.fetcher.artifacts[key]
	art.SHA256 = strings.Repeat("0", 64)
	h.fetcher.artifacts[key] = art
	resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "gemma4:12b"})
	msg := decode[api.Error](t, data).Error.Message
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(msg, "changed upstream") || !strings.Contains(msg, art.SHA256) || !strings.Contains(msg, e.SHA256) {
		t.Fatalf("changed upstream: %d %s", resp.StatusCode, data)
	}
	if h.cat.Store.Exists(e.HFRepo, e.File) {
		t.Fatal("an artifact that does not match its pin must not be downloaded")
	}
}

func TestPullResolveFailuresAreReported(t *testing.T) {
	h := newHarness(t, "")
	e, _ := h.cat.Registry.Find("glm-4.7-flash")
	delete(h.fetcher.artifacts, e.HFRepo+"/"+e.File)
	resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "glm-4.7-flash"})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(data), "has no file "+e.File) {
		t.Fatalf("registry resolve failure: %d %s", resp.StatusCode, data)
	}
	resp, data = h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/missing.gguf"})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(data), "has no file missing.gguf") {
		t.Fatalf("hf resolve failure: %d %s", resp.StatusCode, data)
	}
	if entries, _ := h.cat.Index.Load(); len(entries) != 0 {
		t.Fatalf("a failed hf pull must not be indexed: %+v", entries)
	}
}

func TestAFailedPullFreesItsNameAndIndexesNothing(t *testing.T) {
	h := newHarness(t, "")
	h.fetcher.fail = "extra.gguf"
	resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/extra.gguf"})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(data), "boom") {
		t.Fatalf("failed download: %d %s", resp.StatusCode, data)
	}
	if entries, _ := h.cat.Index.Load(); len(entries) != 0 {
		t.Fatalf("index: %+v", entries)
	}
	h.fetcher.fail = ""
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "hf://org/repo/extra.gguf"}); resp.StatusCode != 200 {
		t.Fatalf("retry: %d", resp.StatusCode)
	}
}

func TestPullKeepsTheModelOnDiskWhenTheEngineCannotRefresh(t *testing.T) {
	h := newHarness(t, "")
	h.fake.Fail("refresh", errors.New(transportErr))
	resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: "glm-4.7-flash"})
	msg := decode[api.Error](t, data).Error.Message
	const want = "glm-4.7-flash is on disk, but the engine backend could not pick it up"
	if resp.StatusCode != http.StatusBadGateway || msg != want {
		t.Fatalf("refresh failure: %d %s", resp.StatusCode, data)
	}
	assertNoTransportText(t, "the wait answer", data)
	e, _ := h.cat.Registry.Find("glm-4.7-flash")
	if !h.cat.Store.Exists(e.HFRepo, e.File) {
		t.Fatal("the download itself succeeded")
	}
	status, events := pullEvents(t, h, "glm-4.7-flash")
	if last := events[len(events)-1]; status != 200 || last.Stage != "error" || last.Error != want {
		t.Fatalf("streamed refresh failure: %d %+v", status, events)
	}
	for _, ev := range events {
		if ev.Stage == "done" && ev.File == "" {
			t.Fatalf("a pull the engine could not pick up must not end with done: %+v", events)
		}
		raw, _ := json.Marshal(ev)
		assertNoTransportText(t, "a streamed event", raw)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "dial tcp 127.0.0.1:41234") {
		t.Fatalf("the real refresh error belongs in the log at error level: %s", logs)
	}
}

func TestPullRefusesALocalFileTheEngineCannotRead(t *testing.T) {
	h := newHarness(t, "")
	locked := filepath.Join(t.TempDir(), "Locked.gguf")
	if err := os.WriteFile(locked, []byte("x"), 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(locked); err == nil {
		_ = f.Close()
		t.Skip("this user can read any file, so no file is unreadable to it")
	}
	for _, path := range []string{"/api/pull", "/api/pull?wait=1"} {
		resp, data := h.do(t, http.MethodPost, path, "", api.PullRequest{Model: locked})
		if body := decode[api.Error](t, data).Error; resp.StatusCode != http.StatusNotFound || !strings.Contains(body.Message, "Locked.gguf is not a readable file") {
			t.Errorf("%s: %d %s", path, resp.StatusCode, data)
		}
	}
	if _, err := os.Lstat(h.cat.Store.Path("local", "Locked.gguf")); !os.IsNotExist(err) {
		t.Fatalf("a link was made to a file the engine cannot read: %v", err)
	}
	if entries, _ := h.cat.Index.Load(); len(entries) != 0 || h.fake.Refreshes != 0 {
		t.Fatalf("a refused import must change nothing: %+v %d", entries, h.fake.Refreshes)
	}
}

// A pull that has passed admission and then cannot record the model ends with an error event. Admission reads the
// index too, so the plans are made by hand here, the way the index looks to a pull that is already under way.
func TestPullFailsWhenTheIndexCannotBeWritten(t *testing.T) {
	h := newHarness(t, "")
	breakIndex(t, h)
	s := newServer(h.deps)
	local := filepath.Join(t.TempDir(), "Mine.gguf")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	plans := map[string]plan{
		"hf":    {ref: pull.Ref{Kind: pull.KindHF, Name: "extra", Repo: "org/repo", File: "extra.gguf"}, release: func() {}},
		"local": {ref: pull.Ref{Kind: pull.KindLocal, Name: "Mine", Path: local}, local: info, release: func() {}},
	}
	for name, p := range plans {
		var events []api.PullEvent
		err := s.runPull(context.Background(), p, func(e api.PullEvent) { events = append(events, e) })
		if err == nil || !strings.Contains(err.Error(), "parse index") {
			t.Errorf("%s: %v", name, err)
		}
		if last := events[len(events)-1]; last.Stage != "error" || !strings.Contains(last.Error, "parse index") {
			t.Errorf("%s: the failure is the last event: %+v", name, events)
		}
	}
}

func TestPullLocalFailsWhenTheLinkIsTaken(t *testing.T) {
	h := newHarness(t, "")
	taken := h.cat.Store.Path("local", "Mine.gguf")
	if err := os.MkdirAll(filepath.Dir(taken), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(taken, []byte("a regular file"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "Mine.gguf")
	if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, data := h.do(t, http.MethodPost, "/api/pull?wait=1", "", api.PullRequest{Model: local})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(data), "left alone") {
		t.Fatalf("taken link: %d %s", resp.StatusCode, data)
	}
	if b, _ := os.ReadFile(taken); string(b) != "a regular file" {
		t.Fatalf("the file in the way was replaced: %q", b)
	}
	if entries, _ := h.cat.Index.Load(); len(entries) != 0 {
		t.Fatalf("index: %+v", entries)
	}
}

func TestPullWritesNothingOutsideTheStore(t *testing.T) {
	h := newHarness(t, "")
	s := newServer(h.deps)
	escape := filepath.Join(h.dataDir, "escape.gguf")
	evil := registry.Entry{Name: "evil", HFRepo: "org/repo", File: "../../../escape.gguf", SHA256: strings.Repeat("a", 64), Size: 8}
	plans := map[string]plan{
		"registry": {ref: pull.Ref{Kind: pull.KindRegistry, Name: "evil"}, entry: evil, release: func() {}},
		"hf":       {ref: pull.Ref{Kind: pull.KindHF, Name: "evil", Repo: "org/repo", File: "../../../escape.gguf"}, release: func() {}},
	}
	for name, p := range plans {
		var events []api.PullEvent
		err := s.runPull(context.Background(), p, func(e api.PullEvent) { events = append(events, e) })
		if err == nil || !strings.Contains(err.Error(), "leave the store") {
			t.Errorf("%s: a path that leaves the store must be refused: %v", name, err)
		}
		if len(events) != 1 || events[0].Stage != "error" || !strings.Contains(events[0].Error, "leave the store") {
			t.Errorf("%s: the refusal is the only event: %+v", name, events)
		}
	}
	if _, err := os.Stat(escape); !os.IsNotExist(err) {
		t.Fatalf("a file was written outside the store: %v", err)
	}
	if h.fake.Refreshes != 0 {
		t.Fatalf("refreshes %d", h.fake.Refreshes)
	}
}

func TestRunPullEndsWithExactlyOneOutcomeEvent(t *testing.T) {
	h := newHarness(t, "")
	s := newServer(h.deps)
	e, _ := h.cat.Registry.Find("glm-4.7-flash")
	var events []api.PullEvent
	err := s.runPull(context.Background(), plan{ref: pull.Ref{Kind: pull.KindRegistry, Name: e.Name}, entry: e, release: func() {}}, func(ev api.PullEvent) { events = append(events, ev) })
	if err != nil || events[len(events)-1] != (api.PullEvent{Stage: "done"}) {
		t.Fatalf("%v %+v", err, events)
	}
	outcomes := 0
	for _, ev := range events {
		if ev.File == "" && (ev.Stage == "done" || ev.Stage == "error") {
			outcomes++
		}
	}
	if outcomes != 1 {
		t.Fatalf("outcome events: %+v", events)
	}
}

func TestPullsAndFailuresAreLoggedWithoutTheKey(t *testing.T) {
	h := newHarness(t, "secret")
	h.fetcher.fail = "Qwen3.6-27B-Q4_K_M.gguf"
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "secret", api.PullRequest{Model: "qwen3.6:27b"}); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("failing pull: %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, http.MethodPost, "/api/pull?wait=1", "secret", api.PullRequest{Model: "glm-4.7-flash"}); resp.StatusCode != 200 {
		t.Fatalf("good pull: %d", resp.StatusCode)
	}
	pulled(t, h, "gemma4:12b")
	if resp, _ := h.do(t, http.MethodDelete, "/api/models/gemma4:12b", "secret", nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	logs := h.logs.String()
	for _, want := range []string{"pull failed", "qwen3.6:27b", "boom", "pulled", "glm-4.7-flash", "deleted", "gemma4:12b"} {
		if !strings.Contains(logs, want) {
			t.Errorf("log lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "secret") {
		t.Fatalf("the api key must never be logged:\n%s", logs)
	}
}

// The last event of a pull, done or error, goes out after the pull has given its name back, so the client's next
// request, such as an rm right after the pull, never meets the pull as still running.
func TestPullGivesItsNameBackBeforeItsLastEvent(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newHarness(t, "")
		s := newServer(h.deps)
		local := filepath.Join(t.TempDir(), "Mine.gguf")
		if err := os.WriteFile(local, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		ref, err := pull.ParseRef(local)
		if err != nil {
			t.Fatal(err)
		}
		p, refused := s.admit(ref)
		if refused != nil {
			t.Fatalf("admit: %+v", refused)
		}
		if fail {
			breakIndex(t, h) // the pull fails when it records the model
		}
		held := func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			_, taken := s.busy["Mine"]
			return taken
		}
		var heldAtTheEnd []bool
		_ = s.runPull(context.Background(), p, func(e api.PullEvent) {
			if e.Stage == stageError || (e.Stage == stageDone && e.File == "") {
				heldAtTheEnd = append(heldAtTheEnd, held())
			}
		})
		if !slices.Equal(heldAtTheEnd, []bool{false}) {
			t.Fatalf("fail=%v: the name was held at the last event: %v", fail, heldAtTheEnd)
		}
	}
}

// A name given back early is given back once: the release that runs when the request ends never frees the claim of
// the request that took the name since.
func TestAClaimIsGivenBackOnce(t *testing.T) {
	s := newServer(newHarness(t, "").deps)
	first, refused := s.claim("Mine", opPull)
	if refused != nil {
		t.Fatal(refused)
	}
	first()
	if _, refused := s.claim("Mine", opDelete); refused != nil {
		t.Fatalf("a given-back name can be claimed: %+v", refused)
	}
	first()
	if _, refused := s.claim("Mine", opPull); refused == nil {
		t.Fatal("the second release of the first claim freed the claim of the delete")
	}
}
