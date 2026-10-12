package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tara-vision/taraengine/internal/gguf/gguftest"
)

func TestFillNativeContextsReadsTheEntriesThatHaveNone(t *testing.T) {
	dir := t.TempDir()
	idx := &Index{Path: filepath.Join(dir, "models", "index.json")}
	plain := filepath.Join(dir, "plain.gguf")
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, e := range []IndexEntry{
		{Name: "short", Path: gguftest.WriteModel(t, dir, "short.gguf", "llama", 2048), Size: 1},
		{Name: "known", Path: gguftest.WriteModel(t, dir, "known.gguf", "llama", 8192), Size: 1, NativeContext: 4096},
		{Name: "plain", Path: plain, Size: 1},
		{Name: "gone", Path: filepath.Join(dir, "gone.gguf"), Size: 1},
	} {
		if err := idx.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	filled, missed, err := idx.FillNativeContexts()
	if err != nil || filled != 1 || len(missed) != 1 || missed["plain"] == nil {
		t.Fatalf("one window read, and only the file that is there but unreadable missed: %d %v %v", filled, missed, err)
	}
	entries, err := idx.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, e := range entries {
		got[e.Name] = e.NativeContext
	}
	if got["short"] != 2048 || got["known"] != 4096 || got["plain"] != 0 || got["gone"] != 0 {
		t.Fatalf("a window already kept is never read again, and the others stay unknown: %v", got)
	}
	again, missedAgain, err := idx.FillNativeContexts()
	if err != nil || again != 0 || missedAgain["plain"] == nil {
		t.Fatalf("an entry left at 0 is read again by the next call: %d %v %v", again, missedAgain, err)
	}
}

func TestFillNativeContextsWritesNothingWhenNothingChanged(t *testing.T) {
	idx := &Index{Path: filepath.Join(t.TempDir(), "models", "index.json")}
	if err := idx.Add(IndexEntry{Name: "gone", Path: filepath.Join(t.TempDir(), "gone.gguf"), Size: 1}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(idx.Path)
	if err != nil {
		t.Fatal(err)
	}
	if filled, missed, err := idx.FillNativeContexts(); err != nil || filled != 0 || len(missed) != 0 {
		t.Fatalf("%d %v %v", filled, missed, err)
	}
	after, err := os.Stat(idx.Path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the index was replaced although nothing changed: %v", err)
	}
}

func TestFillNativeContextsReportsAnIndexItCannotRead(t *testing.T) {
	idx := &Index{Path: filepath.Join(t.TempDir(), "index.json")}
	if err := os.WriteFile(idx.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := idx.FillNativeContexts(); err == nil {
		t.Fatal("a broken index must be reported")
	}
}
