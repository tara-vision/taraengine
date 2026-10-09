package cli

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/api"
	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/pkg/client"
)

// stub is a server that answers every request with one status and body.
func stub(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTheAPIKeyComesFromTheFlagOrTheEnvironment(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) { deps.Config.APIKey = "sesame" })
	host := "--host=" + d.srv.URL
	if _, err := run(t, "list", host); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("without a key: %v", err)
	}
	if _, err := run(t, "list", host, "--api-key", "sesame"); err != nil {
		t.Fatalf("the flag: %v", err)
	}
	t.Setenv("TARAENGINE_API_KEY", "sesame")
	if _, err := run(t, "list", host); err != nil {
		t.Fatalf("the environment: %v", err)
	}
	if _, err := run(t, "list", host, "--api-key", "wrong"); err == nil {
		t.Fatal("the flag must win over the environment")
	}
}

func TestListShowsTheColumnsAndMarksInferredCapabilities(t *testing.T) {
	d := startDaemon(t, nil)
	host := "--host=" + d.srv.URL
	local := filepath.Join(t.TempDir(), "mine.gguf")
	if err := os.WriteFile(local, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "pull", local, host); err != nil || !strings.Contains(out, "done "+local) {
		t.Fatalf("pull of a local file: %v\n%s", err, out)
	}
	out, err := run(t, "list", host)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.Fields(strings.SplitN(out, "\n", 2)[0])
	if strings.Join(header, " ") != "NAME STATUS SIZE TIER CONTEXT CAPABILITIES" {
		t.Fatalf("header %q", header)
	}
	if row := lineWith(out, "gemma4:12b", "available", "7.2 GB", "16", "tools,thinking,vision"); row == "" || strings.Contains(row, "inferred") {
		t.Fatalf("a catalog model:\n%s", out)
	}
	if row := lineWith(out, "mine", "pulled", "(inferred)"); row == "" {
		t.Fatalf("a model with inferred capabilities:\n%s", out)
	}
}

func TestPsAndLoadShowWhatTheModelHolds(t *testing.T) {
	d := startDaemon(t, nil)
	d.install(t, "gemma4:12b")
	d.fake.SetModels(backend.ModelStatus{
		Name: "gemma4:12b", Status: backend.StatusPulled, Context: 4096,
		Memory: &backend.Memory{Bytes: 10 << 30, Source: "measured"},
	})
	host := "--host=" + d.srv.URL
	out, err := run(t, "load", "gemma4:12b", host)
	if want := "loaded gemma4:12b at 4096 tokens of context, 10.0 GiB measured\n"; err != nil || out != want {
		t.Fatalf("load: %v\n%q, want %q", err, out, want)
	}
	out, err = run(t, "ps", host)
	if err != nil || strings.Join(strings.Fields(strings.SplitN(out, "\n", 2)[0]), " ") != "NAME CONTEXT MEMORY" {
		t.Fatalf("ps header: %v\n%s", err, out)
	}
	if row := lineWith(out, "gemma4:12b", "4096", "10.0 GiB measured"); row == "" {
		t.Fatalf("ps row:\n%s", out)
	}
	d.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusLoaded, Context: 4096})
	if out, err = run(t, "ps", host); err != nil || lineWith(out, "gemma4:12b", "4096", "-") == "" {
		t.Fatalf("a model without a figure shows a dash: %v\n%s", err, out)
	}
	if out, err = run(t, "load", "gemma4:12b", host); err != nil || out != "loaded gemma4:12b at 4096 tokens of context\n" {
		t.Fatalf("load without a figure: %v %q", err, out)
	}
}

func TestPsWithNothingLoadedPrintsTheHeaderOnly(t *testing.T) {
	srv, _ := daemon(t)
	out, err := run(t, "ps", "--host="+srv.URL)
	if err != nil || strings.Join(strings.Fields(out), " ") != "NAME CONTEXT MEMORY" {
		t.Fatalf("%v %q", err, out)
	}
}

func TestLoadPassesItsFlagsToTheDaemon(t *testing.T) {
	d := startDaemon(t, nil)
	d.install(t, "gemma4:12b")
	d.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	if _, err := run(t, "load", "gemma4:12b", "--force", "--context", "4096", "--host="+d.srv.URL); err != nil {
		t.Fatal(err)
	}
	if len(d.fake.Loads) != 1 || !d.fake.Loads[0].Options.Force || d.fake.Loads[0].Options.Context != 4096 {
		t.Fatalf("loads %+v", d.fake.Loads)
	}
	if _, err := run(t, "load", "gemma4:12b", "--host="+d.srv.URL); err != nil {
		t.Fatal(err)
	}
	if last := d.fake.Loads[1]; last.Options.Force || last.Options.Context != 0 {
		t.Fatalf("a plain load asks for no force and no window: %+v", last)
	}
}

func TestAFaultyModelIsRefusedWithTheCLIsOwnAdvice(t *testing.T) {
	d := startDaemon(t, nil)
	d.install(t, "gemma4:12b")
	d.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusFaulty})
	d.fake.Fail("load", backend.ErrFaulty)
	_, err := run(t, "load", "gemma4:12b", "--host="+d.srv.URL)
	if err == nil || !strings.Contains(err.Error(), "taraengine load gemma4:12b --force") {
		t.Fatalf("the advice names the flag of this command line: %v", err)
	}
	for _, serverWording := range []string{"a forced load retries it", "load it with force to try again"} {
		if strings.Contains(err.Error(), serverWording) {
			t.Fatalf("the server's message and hint must not reach the user, found %q: %v", serverWording, err)
		}
	}
}

func TestOtherLoadFailuresKeepTheServersWords(t *testing.T) {
	d := startDaemon(t, nil)
	d.install(t, "gemma4:12b")
	d.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
	d.fake.Fail("load", backend.ErrLoadTimeout)
	_, err := run(t, "load", "gemma4:12b", "--host="+d.srv.URL)
	if err == nil || !strings.Contains(err.Error(), "did not load before the timeout") || !strings.Contains(err.Error(), "load_timeout") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "force") {
		t.Fatalf("only a faulty model is advised to be forced: %v", err)
	}
}

func TestRmAndUnloadReportWhatTheDaemonRefuses(t *testing.T) {
	srv, _ := daemon(t)
	host := "--host=" + srv.URL
	if _, err := run(t, "rm", "gemma4:12b", host); err == nil || !strings.Contains(err.Error(), "gemma4:12b is not pulled") {
		t.Fatalf("rm of a model that is not on disk: %v", err)
	}
	if _, err := run(t, "unload", "nope", host); err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("unload of an unknown model: %v", err)
	}
}

func TestAConflictOnDeleteKeepsTheServersWords(t *testing.T) {
	url := stub(t, http.StatusConflict, `{"error":{"message":"a pull of m is running","hint":"try again when it has finished"}}`)
	_, err := run(t, "rm", "m", "--host="+url)
	if err == nil || err.Error() != "a pull of m is running (try again when it has finished)" {
		t.Fatalf("%v", err)
	}
}

func TestACommandThatTakesNoArgumentsRefusesOne(t *testing.T) {
	for _, args := range [][]string{
		{"list", "extra"}, {"ps", "extra"}, {"serve", "extra"}, {"doctor", "extra"},
		{"runtime", "install", "extra"}, {"runtime", "update", "extra"}, {"runtime", "status", "extra"},
	} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"pull"}, {"load"}, {"unload"}, {"rm"}, {"pull", "a", "b"}, {"load", "a", "b"}} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "arg") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestARefusedKeyIsAnsweredWithTheCLIsOwnHint(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) { deps.Config.APIKey = "sesame" })
	host := "--host=" + d.srv.URL
	const want = "missing or wrong API key (" + keyHint + ")"
	for _, args := range [][]string{{"list"}, {"ps"}, {"rm", "m"}, {"load", "m"}, {"unload", "m"}, {"pull", "m"}} {
		_, err := run(t, slices.Concat(args, []string{host})...)
		var refused *client.Error
		if err == nil || err.Error() != want || !errors.As(err, &refused) || refused.Status != http.StatusUnauthorized {
			t.Errorf("%v: got %v, want %q", args, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "Bearer") {
			t.Errorf("%v: the server's Bearer wording must not reach the user: %v", args, err)
		}
	}
	if _, err := run(t, "list", host, "--api-key", "wrong"); err == nil || err.Error() != want {
		t.Errorf("a wrong key is refused the same way: %v", err)
	}
}
