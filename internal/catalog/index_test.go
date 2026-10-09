package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func TestIndexRoundTrip(t *testing.T) {
	idx := &Index{Path: filepath.Join(t.TempDir(), "models", "index.json")}
	entries, err := idx.Load()
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty index: %v, %v", entries, err)
	}
	a := IndexEntry{Name: "stories260K", Repo: "ggml-org/models-moved", File: "tinyllamas/stories260K.gguf", SHA256: "270c", Path: "/data/models/ggml-org/models-moved/tinyllamas/stories260K.gguf", Size: 1185376}
	b := IndexEntry{Name: "mine", Path: "/data/models/local/mine.gguf", Size: 10}
	for _, e := range []IndexEntry{a, b, a} {
		if err := idx.Add(e); err != nil {
			t.Fatal(err)
		}
	}
	entries, err = idx.Load()
	if err != nil || len(entries) != 2 {
		t.Fatalf("after adds: %v, %v", entries, err)
	}
	if entries[0].Name != "mine" || entries[1].Name != "stories260K" {
		t.Fatalf("entries must be sorted by name: %v", entries)
	}
	if err := idx.Remove("mine"); err != nil {
		t.Fatal(err)
	}
	if err := idx.Remove("absent"); err != nil {
		t.Fatal("removing an absent name must not fail")
	}
	entries, _ = idx.Load()
	if len(entries) != 1 || entries[0].Name != "stories260K" {
		t.Fatalf("after remove: %v", entries)
	}
}

func TestIndexRejectsABrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Index{Path: path}).Load(); err == nil {
		t.Fatal("broken index accepted")
	}
}

func TestIndexLoadRejectsADirectory(t *testing.T) {
	if _, err := (&Index{Path: t.TempDir()}).Load(); err == nil {
		t.Fatal("an index path that is a directory must not load")
	}
}

func TestIndexWritesRejectABrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	idx := &Index{Path: path}
	if err := idx.Add(IndexEntry{Name: "mine"}); err == nil {
		t.Fatal("add over a broken index must fail")
	}
	if err := idx.Remove("mine"); err == nil {
		t.Fatal("remove over a broken index must fail")
	}
	if data, _ := os.ReadFile(path); string(data) != "{" {
		t.Fatalf("a failed write changed the broken file: %q", data)
	}
}

func TestIndexAddFailsOnADanglingDirectoryLink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "models")
	if err := os.Symlink(filepath.Join(root, "nowhere"), dir); err != nil {
		t.Fatal(err)
	}
	if err := (&Index{Path: filepath.Join(dir, "index.json")}).Add(IndexEntry{Name: "mine"}); err == nil {
		t.Fatal("an index directory that is a dangling link must fail the write")
	}
}

func TestIndexKeepsEveryParallelAdd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "models")
	idx := &Index{Path: filepath.Join(dir, "index.json")}
	const writers = 24
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for n := 0; n < writers; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			errs <- idx.Add(IndexEntry{Name: fmt.Sprintf("model-%02d", n)})
		}(n)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel add failed: %v", err)
		}
	}
	entries, err := idx.Load()
	if err != nil || len(entries) != writers {
		t.Fatalf("want %d entries, got %d (err %v)", writers, len(entries), err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.Name] = true
	}
	for n := 0; n < writers; n++ {
		if name := fmt.Sprintf("model-%02d", n); !seen[name] {
			t.Fatalf("parallel add lost %s", name)
		}
	}
	if got := entriesIn(t, dir); !slices.Equal(got, []string{"index.json"}) {
		t.Fatalf("parallel writes left files behind: %v", got)
	}
}

func TestIndexSaveFailsWhenTheTargetIsADirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "models")
	target := filepath.Join(dir, "index.json")
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	// save is called directly: Add reads the directory as an index and fails before the rename.
	if err := (&Index{Path: target}).save([]IndexEntry{{Name: "mine"}}); err == nil {
		t.Fatal("renaming onto a directory must fail the write")
	}
	if got := entriesIn(t, dir); !slices.Equal(got, []string{"index.json"}) {
		t.Fatalf("a failed rename left files behind: %v", got)
	}
}

func TestIndexAddFailsInAReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "models")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })
	if err := (&Index{Path: filepath.Join(dir, "index.json")}).Add(IndexEntry{Name: "mine"}); err == nil {
		t.Fatal("writing into a read-only directory must fail")
	}
	if got := entriesIn(t, dir); len(got) != 0 {
		t.Fatalf("a failed write left files behind: %v", got)
	}
}

// entriesIn lists the names in dir, so a test can check that a failed write left nothing behind.
func entriesIn(t *testing.T, dir string) []string {
	t.Helper()
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(dirEntries))
	for _, de := range dirEntries {
		names = append(names, de.Name())
	}
	return names
}
