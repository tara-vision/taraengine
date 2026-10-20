package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tara-vision/taraengine/internal/pull"
)

var (
	modelSHA = strings.Repeat("a1", 32)
	projSHA  = strings.Repeat("b2", 32)
	otherSHA = strings.Repeat("c3", 32)
)

const (
	modelFile = "ggml-org/demo-GGUF/demo-Q4_0.gguf"
	projFile  = "ggml-org/demo-GGUF/mmproj-demo.gguf"
)

// hubFiles is what the stub hub knows: size and LFS sha256 per "owner/repo/path".
var hubFiles = map[string]struct {
	size int64
	sha  string
}{
	modelFile: {size: 1185376, sha: modelSHA},
	projFile:  {size: 5000, sha: projSHA},
}

// hub is a stub of the paths-info endpoint with the shape of the real answer (internal/pull/testdata/paths-info.json).
// A "owner/repo/path" that hubFiles does not hold is a missing repository: 404.
type hub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	asked []string
	auth  []string
}

func newHub(t *testing.T) *hub {
	t.Helper()
	h := &hub{}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	repo, hasPrefix := strings.CutPrefix(r.URL.Path, "/api/models/")
	repo, hasSuffix := strings.CutSuffix(repo, "/paths-info/main")
	var body struct {
		Paths []string `json:"paths"`
	}
	if r.Method != http.MethodPost || !hasPrefix || !hasSuffix ||
		json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Paths) != 1 {
		http.Error(w, "unexpected request", http.StatusBadRequest)
		return
	}
	key := repo + "/" + body.Paths[0]
	h.mu.Lock()
	h.asked, h.auth = append(h.asked, key), append(h.auth, r.Header.Get("Authorization"))
	h.mu.Unlock()
	file, ok := hubFiles[key]
	if !ok {
		http.Error(w, `{"error":"Repository not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode([]map[string]any{{
		"type": "file", "path": body.Paths[0], "size": file.size,
		"lfs": map[string]any{"oid": file.sha, "size": file.size},
	}})
}

// requests lists the files that were asked about, in order.
func (h *hub) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.asked)
}

// tokens lists the Authorization header of each request, in order.
func (h *hub) tokens() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.auth)
}

// resolver is a resolver for the stub, built the way main builds one, through pull.NewResolver.
func (h *hub) resolver(token string) *pull.Resolver {
	r := pull.NewResolver(token)
	r.BaseURL = h.srv.URL
	return r
}

// demoRegistry is a registry of one entry with a projector sibling, with the given values for the two files.
func demoRegistry(sha, size, projSHA, projSize string) string {
	return fmt.Sprintf(`# header comment, kept as written
entries:
  - name: demo:1b
    family: demo
    hf_repo: ggml-org/demo-GGUF
    file: demo-Q4_0.gguf
    sha256: %s
    size: %s
    quantization: Q4_0
    mmproj:
      file: mmproj-demo.gguf
      sha256: %s
      size: %s
    notes: kept as written
`, sha, size, projSHA, projSize)
}

func fillWith(t *testing.T, h *hub, text string) (string, error) {
	t.Helper()
	return fill(context.Background(), h.resolver(""), text)
}

func TestFillPinsAPlaceholderEntryAndItsSibling(t *testing.T) {
	h := newHub(t)
	got, err := fillWith(t, h, demoRegistry("<pin>", "0", "<pin>", "0"))
	if err != nil {
		t.Fatalf("fill: %v", err)
	}
	if want := demoRegistry(modelSHA, "1185376", projSHA, "5000"); got != want {
		t.Fatalf("filled registry wrong:\n%s\nwant:\n%s", got, want)
	}
	if want := []string{modelFile, projFile}; !slices.Equal(h.requests(), want) {
		t.Fatalf("asked %v, want one request per file: %v", h.requests(), want)
	}
}

func TestFillMakesNoRequestForAPinnedRegistry(t *testing.T) {
	h := newHub(t)
	pinned := demoRegistry(otherSHA, "12", otherSHA, "34")
	got, err := fillWith(t, h, pinned)
	if err != nil {
		t.Fatalf("fill: %v", err)
	}
	if got != pinned {
		t.Fatalf("a pinned registry changed:\n%s", got)
	}
	if asked := h.requests(); len(asked) != 0 {
		t.Fatalf("a pinned registry made requests: %v", asked)
	}
}

func TestFillOnlyReplacesWhatIsMissing(t *testing.T) {
	h := newHub(t)
	got, err := fillWith(t, h, demoRegistry(otherSHA, "0", "<pin>", "34"))
	if err != nil {
		t.Fatalf("fill: %v", err)
	}
	// The sha256 of the main file and the size of the projector were pinned already, and stay, whatever the hub says.
	if want := demoRegistry(otherSHA, "1185376", projSHA, "34"); got != want {
		t.Fatalf("filled registry wrong:\n%s\nwant:\n%s", got, want)
	}
}

func TestFillReadsEveryKindOfMissingValue(t *testing.T) {
	cases := []struct {
		name, line, want string
		requests         int
	}{
		{"empty sha256", "sha256:", "sha256: " + modelSHA, 1},
		{"placeholder sha256", "sha256: <pin>", "sha256: " + modelSHA, 1},
		{"empty size", "size:", "size: 1185376", 1},
		{"zero size", "size: 0", "size: 1185376", 1},
		{"placeholder size", "size: <size>", "size: 1185376", 1},
		{"pinned sha256", "sha256: " + otherSHA, "sha256: " + otherSHA, 0},
		{"pinned size", "size: 12", "size: 12", 0},
		{"another key", "quantization: <pin>", "quantization: <pin>", 0},
	}
	const head = "  - hf_repo: ggml-org/demo-GGUF\n    file: demo-Q4_0.gguf\n    "
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			got, err := fillWith(t, h, head+tc.line+"\n")
			if err != nil {
				t.Fatalf("fill: %v", err)
			}
			if want := head + tc.want + "\n"; got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			if n := len(h.requests()); n != tc.requests {
				t.Fatalf("%d requests, want %d", n, tc.requests)
			}
		})
	}
}

func TestFillRefusesAValueThatComesBeforeItsFile(t *testing.T) {
	cases := []struct{ name, text string }{
		{"no entry at all", "sha256: <pin>\n"},
		{"before the file", "  - name: a\n    hf_repo: ggml-org/demo-GGUF\n    sha256: <pin>\n    file: demo-Q4_0.gguf\n"},
		{"before the repository", "  - name: a\n    file: demo-Q4_0.gguf\n    sha256: <pin>\n"},
		{"the previous entry's file does not carry over", demoRegistry(otherSHA, "12", otherSHA, "34") +
			"  - name: b\n    hf_repo: ggml-org/demo-GGUF\n    sha256: <pin>\n"},
		{"a new entry starts without the previous entry's repository", demoRegistry(otherSHA, "12", otherSHA, "34") +
			"  - name: b\n    sha256: <pin>\n"},
		{"a new hf_repo drops the previous file", "  - hf_repo: ggml-org/demo-GGUF\n    file: demo-Q4_0.gguf\n" +
			"    sha256: " + otherSHA + "\n  - hf_repo: ggml-org/demo-GGUF\n    sha256: <pin>\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHub(t)
			_, err := fillWith(t, h, tc.text)
			if err == nil || !strings.Contains(err.Error(), "comes before the hf_repo and file of its entry") {
				t.Fatalf("want the refusal, got %v", err)
			}
			if asked := h.requests(); len(asked) != 0 {
				t.Fatalf("a refused value made requests: %v", asked)
			}
		})
	}
}

func TestFillNamesTheLineOfAFailure(t *testing.T) {
	h := newHub(t)
	text := "entries:\n  - name: a\n    hf_repo: ggml-org/absent-GGUF\n    file: a.gguf\n    sha256: <pin>\n"
	_, err := fillWith(t, h, text)
	if err == nil {
		t.Fatal("a missing repository was pinned")
	}
	for _, want := range []string{"pinregistry: line 5", "ggml-org/absent-GGUF", "answered 404"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
}

// runIn writes text to a registry file with the given mode, runs the command on it and returns the exit status, what
// it printed, and the file's content afterwards.
func runIn(t *testing.T, h *hub, text string, mode os.FileMode, env map[string]string) (status int, stderr, after string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	status = run([]string{path}, func(k string) string { return env[k] }, &out, h.resolver)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return status, out.String(), string(data)
}

func TestRunPinsTheFileWithTheTokenFromTheEnvironment(t *testing.T) {
	h := newHub(t)
	env := map[string]string{"TARAENGINE_HF_TOKEN": "secret"}
	status, stderr, after := runIn(t, h, demoRegistry("<pin>", "0", "<pin>", "0"), 0o644, env)
	if status != 0 || stderr != "" {
		t.Fatalf("status %d, stderr %q", status, stderr)
	}
	if want := demoRegistry(modelSHA, "1185376", projSHA, "5000"); after != want {
		t.Fatalf("file after the run:\n%s\nwant:\n%s", after, want)
	}
	if want := []string{"Bearer secret", "Bearer secret"}; !slices.Equal(h.tokens(), want) {
		t.Fatalf("tokens sent %v, want %v", h.tokens(), want)
	}
}

func TestRunNeedsExactlyOneFile(t *testing.T) {
	for _, args := range [][]string{nil, {"a.yaml", "b.yaml"}} {
		var out bytes.Buffer
		if status := run(args, os.Getenv, &out, pull.NewResolver); status != statusUsage {
			t.Errorf("args %v: status %d, want %d", args, status, statusUsage)
		}
		if got, want := out.String(), "usage: pinregistry <registry.yaml>\n"; got != want {
			t.Errorf("args %v: printed %q, want %q", args, got, want)
		}
	}
}

func TestRunReportsAFileItCannotRead(t *testing.T) {
	var out bytes.Buffer
	missing := filepath.Join(t.TempDir(), "absent.yaml")
	if status := run([]string{missing}, os.Getenv, &out, pull.NewResolver); status != statusFailed {
		t.Fatalf("status %d, want %d", status, statusFailed)
	}
	if !strings.Contains(out.String(), "absent.yaml") {
		t.Fatalf("the report does not name the file: %q", out.String())
	}
}

func TestRunReportsAFailedResolveAndLeavesTheFileAlone(t *testing.T) {
	h := newHub(t)
	text := "  - name: a\n    hf_repo: ggml-org/absent-GGUF\n    file: a.gguf\n    size: 0\n"
	status, stderr, after := runIn(t, h, text, 0o644, nil)
	if status != statusFailed || !strings.Contains(stderr, "pinregistry: line 4") {
		t.Fatalf("status %d, stderr %q", status, stderr)
	}
	if after != text {
		t.Fatalf("a failed run changed the file:\n%s", after)
	}
}

func TestRunDoesNotWriteAFileThatIsAlreadyPinned(t *testing.T) {
	h := newHub(t)
	pinned := demoRegistry(otherSHA, "12", otherSHA, "34")
	// A read-only file: any write would fail, so a clean run proves there was none (root ignores the mode, and then
	// the check below on the content is all that is left).
	status, stderr, after := runIn(t, h, pinned, 0o400, nil)
	if status != 0 || stderr != "" || after != pinned {
		t.Fatalf("status %d, stderr %q, file changed: %v", status, stderr, after != pinned)
	}
}

func TestRunReportsAFileItCannotWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	h := newHub(t)
	text := demoRegistry("<pin>", "0", "<pin>", "0")
	status, stderr, after := runIn(t, h, text, 0o400, nil)
	if status != statusFailed || !strings.Contains(stderr, "permission denied") {
		t.Fatalf("status %d, stderr %q", status, stderr)
	}
	if after != text {
		t.Fatalf("a failed write changed the file:\n%s", after)
	}
}

func TestMainExitsWithTheStatusOfRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.yaml")
	if err := os.WriteFile(path, []byte(demoRegistry(otherSHA, "12", otherSHA, "34")), 0o600); err != nil {
		t.Fatal(err)
	}
	previousArgs, previousExit := os.Args, exit
	t.Cleanup(func() { os.Args, exit = previousArgs, previousExit })
	var codes []int
	exit = func(code int) { codes = append(codes, code) }
	// A pinned registry needs no request, so main can run with the real resolver and no network.
	os.Args = []string{"pinregistry", path}
	main()
	os.Args = []string{"pinregistry"}
	main()
	if want := []int{0, statusUsage}; !slices.Equal(codes, want) {
		t.Fatalf("main exited with %v, want %v", codes, want)
	}
}

// drafterFile is a drafter sibling in a repository of its own, which the stub hub knows for the tests below.
const drafterFile = "nvidia/drafter-GGUF/draft.gguf"

func withDrafter(t *testing.T) {
	t.Helper()
	hubFiles[drafterFile] = struct {
		size int64
		sha  string
	}{size: 777, sha: otherSHA}
	t.Cleanup(func() { delete(hubFiles, drafterFile) })
}

func TestFillPinsASiblingInItsOwnRepository(t *testing.T) {
	withDrafter(t)
	h := newHub(t)
	text := strings.Replace(demoRegistry(modelSHA, "1185376", projSHA, "5000"), "    notes: kept as written\n",
		"    dflash:\n      hf_repo: nvidia/drafter-GGUF\n      file: draft.gguf\n      sha256: <pin>\n      size: 0\n    notes: kept as written\n", 1)
	out, err := fillWith(t, h, text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "      sha256: "+otherSHA+"\n      size: 777\n") {
		t.Fatalf("the drafter was not pinned from its own repository:\n%s", out)
	}
	if got := h.requests(); !slices.Equal(got, []string{drafterFile}) {
		t.Fatalf("asked %v, want the drafter under its own repository only", got)
	}
}

// YAML allows any key order: a sibling block with a repository of its own may stand before the entry's own hf_repo
// line, and the walker tells them apart by indentation, not by which comes first.
func TestFillTakesTheEntryRepositoryByIndentation(t *testing.T) {
	withDrafter(t)
	h := newHub(t)
	text := `entries:
  - name: demo:1b
    dflash:
      hf_repo: nvidia/drafter-GGUF
      file: draft.gguf
      sha256: ` + otherSHA + `
      size: 777
    family: demo
    hf_repo: ggml-org/demo-GGUF
    file: demo-Q4_0.gguf
    sha256: <pin>
    size: 0
    mmproj:
      file: mmproj-demo.gguf
      sha256: <pin>
      size: 0
`
	out, err := fillWith(t, h, text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "    sha256: "+modelSHA+"\n    size: 1185376\n") || !strings.Contains(out, "      sha256: "+projSHA+"\n      size: 5000\n") {
		t.Fatalf("the model and the projector were not pinned against the entry's repository:\n%s", out)
	}
	if got := h.requests(); !slices.Equal(got, []string{modelFile, projFile}) {
		t.Fatalf("asked %v, want the model and the projector under the entry's repository", got)
	}
}

func TestFillResolvesASiblingAfterAnotherRepositoryAgainstTheEntry(t *testing.T) {
	withDrafter(t)
	h := newHub(t)
	text := `entries:
  - name: demo:1b
    family: demo
    hf_repo: ggml-org/demo-GGUF
    file: demo-Q4_0.gguf
    sha256: ` + modelSHA + `
    size: 1185376
    dflash:
      hf_repo: nvidia/drafter-GGUF
      file: draft.gguf
      sha256: ` + otherSHA + `
      size: 777
    mmproj:
      file: mmproj-demo.gguf
      sha256: <pin>
      size: 0
`
	out, err := fillWith(t, h, text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "      sha256: "+projSHA+"\n      size: 5000\n") {
		t.Fatalf("the projector was not pinned:\n%s", out)
	}
	if got := h.requests(); !slices.Equal(got, []string{projFile}) {
		t.Fatalf("asked %v, want the projector under the entry's repository, not the drafter's", got)
	}
}
