package llamaserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
)

// writeChildLog puts a log of n numbered lines where the child's log would be and returns the lines.
func writeChildLog(t *testing.T, dataDir string, n int) []string {
	t.Helper()
	lines := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		lines = append(lines, "llama-server line "+strconv.Itoa(1000+i))
	}
	if err := os.MkdirAll(filepath.Dir(LogPath(dataDir)), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, LogPath(dataDir), strings.Join(lines, "\n")+"\n")
	return lines
}

func TestModelsCarryTheEndOfTheLogOfFailedModels(t *testing.T) {
	h := newHarness(t, 0)
	lines := writeChildLog(t, h.b.opts.DataDir, 100)
	want := strings.Join(lines[60:], "\n")
	h.fake.FailLoad("b", 1)
	ctx := context.Background()
	if err := h.b.Load(ctx, "b", backend.LoadOptions{}); !errors.Is(err, backend.ErrLoadFailed) {
		t.Fatalf("first failure: %v", err)
	}
	if b := statusOf(t, h.b, "b"); b.Status != backend.StatusFailed || b.LogTail != want {
		t.Fatalf("a failed model carries the last 40 lines: status %s, tail %q", b.Status, b.LogTail)
	}
	if err := h.b.Load(ctx, "b", backend.LoadOptions{}); !errors.Is(err, backend.ErrLoadFailed) {
		t.Fatalf("second failure: %v", err)
	}
	if b := statusOf(t, h.b, "b"); b.Status != backend.StatusFaulty || b.LogTail != want {
		t.Fatalf("a faulty model carries the tail too: status %s, tail %q", b.Status, b.LogTail)
	}
	if a := statusOf(t, h.b, "a"); a.LogTail != "" {
		t.Fatalf("a model that did not fail has no tail: %q", a.LogTail)
	}
}

func TestModelsOfAFailedModelNeedNoLog(t *testing.T) {
	h := newHarness(t, 0)
	h.fake.FailLoad("b", 1)
	if err := h.b.Load(context.Background(), "b", backend.LoadOptions{}); !errors.Is(err, backend.ErrLoadFailed) {
		t.Fatalf("failure: %v", err)
	}
	if b := statusOf(t, h.b, "b"); b.Status != backend.StatusFailed || b.LogTail != "" {
		t.Fatalf("an attached router has no log here, and that is not an error: %+v", b)
	}
}

func TestLoadFailureNamesTheContextItRanAt(t *testing.T) {
	h := newHarness(t, 0)
	withModel("c")(h)
	if err := h.b.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.fake.FailLoad("b", 1)
	h.fake.FailLoad("c", 3)
	ctx := context.Background()
	rows := []struct {
		name string
		opts backend.LoadOptions
		want string
	}{
		{"b", backend.LoadOptions{}, "exit code 1 at context 4096"},
		{"b", backend.LoadOptions{Context: 8192}, "exit code 1 at context 8192"},
		{"c", backend.LoadOptions{}, "exit code 3 at the model's default context"},
	}
	for _, row := range rows {
		err := h.b.Load(ctx, row.name, row.opts)
		if !errors.Is(err, backend.ErrLoadFailed) || !strings.Contains(err.Error(), row.want) {
			t.Errorf("load %s %+v: want %q in %v", row.name, row.opts, row.want, err)
		}
	}
}

func TestReadLogTail(t *testing.T) {
	dir := t.TempDir()
	logFile := func(content string) string {
		path := filepath.Join(dir, "log-"+strconv.Itoa(len(content))+"-"+strings.ReplaceAll(t.Name(), "/", "_"))
		writeTestFile(t, path, content)
		return path
	}
	rows := []struct {
		name     string
		content  string
		maxBytes int64
		maxLines int
		want     string
	}{
		{"an empty file", "", 1024, 3, ""},
		{"fewer lines than the limit", "a\nb\n", 1024, 3, "a\nb"},
		{"a last line without a newline", "a\nb", 1024, 3, "a\nb"},
		{"exactly the limit", "a\nb\nc\n", 1024, 3, "a\nb\nc"},
		{"more lines than the limit", "1\n2\n3\n4\n5\n", 1024, 3, "3\n4\n5"},
		{"a window that opens inside a line", "alpha\nbeta\ngamma\n", 10, 10, "gamma"},
		{"a window that opens on a line break", "alpha\nbeta\ngamma\n", 12, 10, "beta\ngamma"},
		{"one line longer than the window", strings.Repeat("x", 100), 10, 3, strings.Repeat("x", 10)},
		{"blank lines count as lines", "a\n\n\nb\n", 1024, 3, "\n\nb"},
		{"control characters dropped, line feeds and tabs kept", "a\x1b[31mred\x1b[0m\r\n\u009b2J\tb\x07\n", 1024, 3,
			"a[31mred[0m\n2J\tb"},
		{"a byte that is no character", "a\x9bb\n", 1024, 3, "a\ufffdb"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := readLogTail(logFile(row.content), row.maxBytes, row.maxLines); got != row.want {
				t.Fatalf("got %q, want %q", got, row.want)
			}
		})
	}
}

func TestReadLogTailGivesNothingWhenTheLogCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	if got := readLogTail(filepath.Join(dir, "absent.log"), 1024, 3); got != "" {
		t.Fatalf("a missing log: %q", got)
	}
	if got := readLogTail(dir, 1024, 3); got != "" {
		t.Fatalf("a directory where the log should be: %q", got)
	}
	locked := filepath.Join(dir, "locked.log")
	writeTestFile(t, locked, "secret\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 { // root reads anything
		if got := readLogTail(locked, 1024, 3); got != "" {
			t.Fatalf("an unreadable log: %q", got)
		}
	}
}

func TestModelsGiveEveryFailedModelTheSameLogTail(t *testing.T) {
	h := newHarness(t, 0)
	withModel("c")(h)
	if err := h.b.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeChildLog(t, h.b.opts.DataDir, 3)
	h.fake.FailLoad("b", 1)
	h.fake.FailLoad("c", 1)
	ctx := context.Background()
	for _, name := range []string{"b", "c"} {
		if err := h.b.Load(ctx, name, backend.LoadOptions{}); !errors.Is(err, backend.ErrLoadFailed) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	want := "llama-server line 1001\nllama-server line 1002\nllama-server line 1003"
	for _, name := range []string{"b", "c"} {
		if m := statusOf(t, h.b, name); m.LogTail != want {
			t.Errorf("%s: %q", name, m.LogTail)
		}
	}
}
