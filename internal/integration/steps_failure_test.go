//go:build integration

package integration

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/pkg/client"
)

// damagedFile stands in for a model file that was cut off or overwritten: the runtime cannot read it.
const damagedFile = "this is not a gguf file"

// checkFailedLoadThenRetry damages the pulled file, so that the runtime refuses it and exits, and asserts that the
// failure shows everywhere it should. It then repairs the file and loads again. The retry only works when the router
// forgets the failure as the new load starts, which the backend relies on. One failure is not two: the model is
// not marked faulty, so the retry needs no force. The model is left loaded.
func checkFailedLoadThenRetry(t *testing.T, s *scenario) {
	path := s.modelPath()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(damagedFile), info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}

	_, err = s.c.Load(within(t, loadTimeout), s.entry.Name, 0, false)
	var refused *client.Error
	if !errors.As(err, &refused) || refused.Status != http.StatusBadGateway ||
		!strings.Contains(refused.Message, "exit code 1") {
		t.Fatalf("load of a damaged file: want 502 naming the exit code, got %v", err)
	}
	assertFailureShows(t, s)

	if err := os.WriteFile(path, original, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	m, err := s.c.Load(within(t, loadTimeout), s.entry.Name, 0, false)
	if err != nil || m.Status != "loaded" || m.ExitCode != 0 || m.Error != "" {
		t.Fatalf("the retry after the repair: %+v %v", m, err)
	}
	if again := s.model(t); again.Status != "loaded" || again.ExitCode != 0 || again.Error != "" {
		t.Fatalf("the failure was not cleared by the retry: %+v", again)
	}
	assertListedAt(t, s, contextWindow)
	s.complete(t)
}

// assertFailureShows asserts what a failed load leaves behind: nothing loaded, a failed status with the exit code, the
// end of the runtime's log naming the file it could not read, and the same in the output of doctor.
func assertFailureShows(t *testing.T, s *scenario) {
	t.Helper()
	if loaded, err := s.c.Loaded(within(t, callTimeout)); err != nil || len(loaded) != 0 {
		t.Fatalf("loaded after a failed load: %+v %v", loaded, err)
	}
	if m := s.model(t); m.Status != "failed" || m.ExitCode != 1 || m.Error == "" {
		t.Fatalf("after a failed load: %+v", m)
	}
	file := filepath.Base(s.modelPath())
	detail, err := s.c.Model(within(t, callTimeout), s.entry.Name)
	if err != nil || !strings.Contains(detail.LogTail, file) {
		t.Fatalf("the log tail does not name %s: %q %v", file, detail.LogTail, err)
	}
	out := succeeded(t, s.d.cli(t, "doctor"), "doctor")
	if want := "Model " + s.entry.Name + ": failed, exit code 1"; !strings.Contains(out, want) || !strings.Contains(out, file) {
		t.Fatalf("doctor does not report the failure (want %q and the log tail):\n%s", want, out)
	}
}
