package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/api"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	apitypes "github.com/tara-vision/taraengine/pkg/api"
	"github.com/tara-vision/taraengine/pkg/client"
)

// scriptFetcher plays a download: it reports the given progress, then writes the file or fails.
type scriptFetcher struct {
	cliFetcher
	progress []pull.Progress
	err      error
}

func (f scriptFetcher) Fetch(_ context.Context, _ pull.Artifact, dest string, onProgress func(pull.Progress)) error {
	for _, p := range f.progress {
		onProgress(p)
	}
	if f.err != nil {
		return f.err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	return os.WriteFile(dest, []byte("x"), 0o600)
}

// refusingFetcher cannot resolve anything: Hugging Face turns the request down.
type refusingFetcher struct {
	cliFetcher
	err error
}

func (f refusingFetcher) Resolve(context.Context, string, string) (pull.Artifact, error) {
	return pull.Artifact{}, f.err
}

func registryFile(t *testing.T, name string) string {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Find(name)
	if !ok {
		t.Fatalf("%s is not in the registry", name)
	}
	return entry.File
}

func TestPullShowsEachStageAndOneLineEveryFivePercent(t *testing.T) {
	const gigabyte = 1_000_000_000
	file := registryFile(t, "glm-4.7-flash")
	d := startDaemon(t, func(deps *api.Deps) {
		deps.Fetcher = scriptFetcher{progress: []pull.Progress{
			{Stage: "downloading", Bytes: 0, Total: 4 * gigabyte},
			{Stage: "downloading", Bytes: 1, Total: 4 * gigabyte},
			{Stage: "downloading", Bytes: gigabyte, Total: 4 * gigabyte},
			{Stage: "downloading", Bytes: gigabyte + 1, Total: 4 * gigabyte},
			{Stage: "downloading", Bytes: 3 * gigabyte, Total: 4 * gigabyte},
			{Stage: "downloading", Bytes: 4 * gigabyte, Total: 4 * gigabyte},
			{Stage: "downloading", Bytes: 5, Total: 0},
			{Stage: "verifying", Bytes: 4 * gigabyte, Total: 4 * gigabyte},
			{Stage: "done", Bytes: 4 * gigabyte, Total: 4 * gigabyte},
		}}
	})
	out, err := run(t, "pull", "glm-4.7-flash", "--host="+d.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"resolving " + file,
		"downloading " + file + " 1.0 GB / 4.0 GB",
		"downloading " + file + " 3.0 GB / 4.0 GB",
		"downloading " + file + " 4.0 GB / 4.0 GB",
		"verifying " + file,
		"done " + file,
		"done glm-4.7-flash",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("output\n%s\nwant\n%s", out, want)
	}
}

func TestPullOfAFileAlreadyOnDiskReportsItDone(t *testing.T) {
	d := startDaemon(t, nil)
	d.install(t, "glm-4.7-flash")
	out, err := run(t, "pull", "glm-4.7-flash", "--host="+d.srv.URL)
	if want := "done " + registryFile(t, "glm-4.7-flash") + "\ndone glm-4.7-flash\n"; err != nil || out != want {
		t.Fatalf("%v\n%q, want %q", err, out, want)
	}
}

func TestPullWithoutAnythingToDownloadWithALocalFile(t *testing.T) {
	d := startDaemon(t, nil)
	local := filepath.Join(t.TempDir(), "mine.gguf")
	if err := os.WriteFile(local, []byte("0123"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "pull", local, "--host="+d.srv.URL)
	if err != nil || out != "done "+local+"\n" {
		t.Fatalf("%v %q", err, out)
	}
}

func TestPullReportsAFailureOnce(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) {
		deps.Fetcher = scriptFetcher{err: errors.New("pull: no space left")}
	})
	out, err := run(t, "pull", "glm-4.7-flash", "--host="+d.srv.URL)
	if err == nil || err.Error() != "pull: no space left" {
		t.Fatalf("%v", err)
	}
	if strings.Contains(out, "no space left") {
		t.Fatalf("the error is reported by the returned error, not twice:\n%s", out)
	}
}

func TestPullRefusedByHuggingFaceHintsAtTheTokenSetting(t *testing.T) {
	const secret = "hf_SECRET_VALUE_1234"
	cfgFile := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("hf_token: "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TARAENGINE_HF_TOKEN", secret)
	for _, status := range []int{401, 403} {
		cause := fmt.Errorf("pull: resolve ggml-org/x: Hugging Face answered %d", status)
		d := startDaemon(t, func(deps *api.Deps) { deps.Fetcher = refusingFetcher{err: cause} })
		out, err := run(t, "pull", "glm-4.7-flash", "--host="+d.srv.URL, "--config", cfgFile)
		if err == nil || !strings.Contains(err.Error(), cause.Error()) {
			t.Fatalf("%d: the cause stays in the message: %v", status, err)
		}
		for _, want := range []string{"hf_token", cfgFile, "TARAENGINE_HF_TOKEN", "restart"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%d: the hint must mention %q: %v", status, want, err)
			}
		}
		if strings.Contains(err.Error()+out, secret) {
			t.Fatalf("%d: the token must never be printed", status)
		}
	}
}

func TestPullHintNamesTheDefaultConfigFileWithoutAFlag(t *testing.T) {
	cause := errors.New("pull: download x.gguf: Hugging Face answered 403")
	d := startDaemon(t, func(deps *api.Deps) { deps.Fetcher = refusingFetcher{err: cause} })
	_, err := run(t, "pull", "glm-4.7-flash", "--host="+d.srv.URL)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "taraengine", "config.yaml")) {
		t.Fatalf("%v", err)
	}
}

func TestPullOtherRefusalsGetNoTokenHint(t *testing.T) {
	for _, status := range []int{404, 500} {
		cause := fmt.Errorf("pull: resolve ggml-org/x: Hugging Face answered %d", status)
		d := startDaemon(t, func(deps *api.Deps) { deps.Fetcher = refusingFetcher{err: cause} })
		_, err := run(t, "pull", "glm-4.7-flash", "--host="+d.srv.URL)
		if err == nil || err.Error() != cause.Error() {
			t.Fatalf("%d: %v", status, err)
		}
	}
}

func TestPullConflictsKeepTheServersWordsAndNeverSuggestForce(t *testing.T) {
	srv, _ := daemon(t)
	_, err := run(t, "pull", "hf://org/repo/gemma4:12b.gguf", "--host="+srv.URL)
	want := "the name gemma4:12b belongs to the catalog (pull it by that name)"
	if err == nil || err.Error() != want {
		t.Fatalf("%v, want %q", err, want)
	}
	if _, err := run(t, "pull", "no-such-model", "--host="+srv.URL); err == nil || !strings.Contains(err.Error(), "unknown catalog name: no-such-model") {
		t.Fatalf("%v", err)
	}
}

// cutStream answers a pull with one event and then drops the connection.
func cutStream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"stage\":\"resolving\",\"file\":\"m.gguf\"}\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestPullCutOffMidwayIsNotAMissingDaemon(t *testing.T) {
	url := cutStream(t)
	out, err := run(t, "pull", "m", "--host="+url)
	if err == nil || !errors.Is(err, client.ErrIncompleteStream) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(err.Error(), "the stream ended before the pull finished") ||
		!strings.Contains(err.Error(), "run the same pull again to resume") {
		t.Fatalf("the user is told what happened and what to do: %v", err)
	}
	if strings.Contains(err.Error(), "is not running") {
		t.Fatalf("the engine was reachable: %v", err)
	}
	if out != "resolving m.gguf\n" {
		t.Fatalf("what arrived before the cut is still shown: %q", out)
	}
}

// endedStream answers a pull with the event of one file and then ends the stream cleanly, without the overall done
// event that closes a pull.
func endedStream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"stage\":\"done\",\"file\":\"m.gguf\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestPullThatEndsBeforeTheOverallDoneIsNotAFinishedPull(t *testing.T) {
	url := endedStream(t)
	out, err := run(t, "pull", "m", "--host="+url)
	if err == nil || !errors.Is(err, client.ErrIncompleteStream) || !strings.Contains(err.Error(), "run the same pull again to resume") {
		t.Fatalf("%v", err)
	}
	if out != "done m.gguf\n" {
		t.Fatalf("a finished file is not a finished pull, and the CLI says so only once it is: %q", out)
	}
}

func TestPullAnswerThatIsNotAPullStreamIsAnError(t *testing.T) {
	url := stub(t, http.StatusOK, `{"hello":"world"}`)
	_, err := run(t, "pull", "m", "--host="+url)
	if err == nil || !errors.Is(err, client.ErrIncompleteStream) || !strings.Contains(err.Error(), "not an event stream") {
		t.Fatalf("%v", err)
	}
}

func TestPullStreamWithATooLongEventIsReportedAsItIs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+strings.Repeat("x", 2<<20)+"\n\n")
	}))
	t.Cleanup(srv.Close)
	_, err := run(t, "pull", "m", "--host="+srv.URL)
	if err == nil || errors.Is(err, client.ErrIncompleteStream) || strings.Contains(err.Error(), "run the same pull again") {
		t.Fatalf("a daemon that sends nonsense is not a stream that ended early: %v", err)
	}
}

func TestProgressLinesCountSmallFilesInMegabytes(t *testing.T) {
	var out strings.Builder
	p := &printer{w: &out}
	progressLines(p, "m")(apitypes.PullEvent{Stage: "downloading", File: "mmproj.gguf", Bytes: 5_500_000, Total: 11_000_000})
	downloadLines(p)(pull.Progress{Bytes: 11_000_000, Total: 11_000_000})
	want := "downloading mmproj.gguf 5.5 MB / 11.0 MB\ndownloading 11.0 MB / 11.0 MB\n"
	if out.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", out.String(), want)
	}
}
