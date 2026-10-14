package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/api"
	apitypes "github.com/tara-vision/taraengine/pkg/api"
)

func TestDoctorReportsTheRuntimeTheDaemonRuns(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) {
		deps.Runtime = func() apitypes.RuntimeStatus {
			return apitypes.RuntimeStatus{Installed: true, Verified: true, Build: "b11514", Dir: "/var/lib/taraengine/runtime/b11514"}
		}
	})
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	want := "Runtime: b11514 verified in /var/lib/taraengine/runtime/b11514 (as the engine at " + d.srv.URL + " sees it)"
	if err != nil || !strings.Contains(out, want) || strings.Contains(out, "runtime install") {
		t.Fatalf("the runtime line is the daemon's, not this user's empty data directory: %v\n%s", err, out)
	}
}

func TestDoctorReportsARuntimeTheDaemonCannotVerify(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) {
		deps.Runtime = func() apitypes.RuntimeStatus {
			return apitypes.RuntimeStatus{Installed: true, Build: "b11514", Error: "llama-server does not match the manifest"}
		}
	})
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil || !strings.Contains(out, "Runtime: b11514 llama-server does not match the manifest (as the engine at") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestDoctorReportsAStartingEngineAtOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/health":
			_, _ = w.Write([]byte(`{"status":"starting","backend":"llama.cpp","detail":"installing runtime b11514 (40%)"}`))
		case "/api/version":
			_, _ = w.Write([]byte(`{"version":"0.3.0","runtime":{"build":"b11514","backend":"llama.cpp"},"platform":"linux/amd64/cuda12"}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"taraengine is starting: installing runtime b11514 (40%)"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	out, err := run(t, "doctor", "--host="+srv.URL, "--data-dir", t.TempDir())
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Runtime: installing runtime b11514 (40%) (the engine at " + srv.URL + " is starting)",
		"Daemon: taraengine 0.3.0 on linux/amd64/cuda12, runtime b11514, at " + srv.URL + ": starting: installing runtime b11514 (40%)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "Loaded:") {
		t.Errorf("a starting engine is not asked for its loaded models:\n%s", out)
	}
}

func TestDoctorLooksAtThisUsersRuntimeOnlyWhenNoEngineAnswers(t *testing.T) {
	d := startDaemon(t, func(deps *api.Deps) { deps.Config.APIKey = "sesame" }) // a system service with a key
	out, err := run(t, "doctor", "--host="+d.srv.URL, "--data-dir", t.TempDir())
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "runtime install") || !strings.Contains(out, "Runtime: the engine at "+d.srv.URL+" did not say which:") {
		t.Fatalf("an engine that refuses the key still runs the runtime that counts, not this user's empty data directory:\n%s", out)
	}
	out, err = run(t, "doctor", "--host=http://127.0.0.1:1", "--data-dir", t.TempDir())
	if err != nil || !strings.Contains(out, "runtime install") {
		t.Fatalf("with no engine answering, this user's data directory is the one that counts: %v\n%s", err, out)
	}
}
