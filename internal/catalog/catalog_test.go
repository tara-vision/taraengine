package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
)

func testCatalog(t *testing.T) (Catalog, pull.Store) {
	t.Helper()
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	store := pull.Store{Dir: filepath.Join(t.TempDir(), "models")}
	return Catalog{Registry: reg, Store: store, Index: &Index{Path: filepath.Join(store.Dir, "index.json")}}, store
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

//nolint:gocyclo // one test checks every kind of joined item in one pass
func TestListJoinsRegistryStoreAndIndex(t *testing.T) {
	c, store := testCatalog(t)
	gemma, _ := c.Registry.Find("gemma4:12b")
	touch(t, store.Path(gemma.HFRepo, gemma.File))
	touch(t, store.Path(gemma.HFRepo, gemma.MTP.File))
	local := filepath.Join(t.TempDir(), "mine.gguf")
	touch(t, local)
	link, err := store.LinkLocal("mine", local)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Index.Add(IndexEntry{Name: "mine", Path: link, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.Index.Add(IndexEntry{Name: "ghost", Repo: "org/repo", File: "ghost.gguf", Path: store.Path("org/repo", "ghost.gguf"), Size: 1}); err != nil {
		t.Fatal(err)
	}

	items, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(c.Registry.Entries)+2 {
		t.Fatalf("%d items", len(items))
	}
	byName := map[string]Item{}
	for _, it := range items {
		byName[it.Name] = it
	}
	g := byName["gemma4:12b"]
	if !g.Pulled || g.Entry == nil || g.ModelPath != store.Path(gemma.HFRepo, gemma.File) || g.DraftPath == "" || g.MMProjPath != "" {
		t.Fatalf("gemma item wrong: %+v", g)
	}
	if g.Size != gemma.Size || g.Inferred {
		t.Fatalf("gemma size or inferred wrong: %+v", g)
	}
	if q := byName["qwen3.6:35b"]; q.Pulled || q.ModelPath == "" {
		t.Fatalf("unpulled entry wrong: %+v", q)
	}
	if m := byName["mine"]; !m.Pulled || !m.Local || !m.Inferred || m.Entry != nil || m.ModelPath != link {
		t.Fatalf("local item wrong: %+v", m)
	}
	if gh := byName["ghost"]; gh.Pulled || gh.Local {
		t.Fatalf("index entry without a file must show unpulled: %+v", gh)
	}

	it, ok, err := c.Find("mine")
	if err != nil || !ok || it.Name != "mine" {
		t.Fatalf("find mine: %+v %v %v", it, ok, err)
	}
	if _, ok, _ := c.Find("absent"); ok {
		t.Fatal("found an absent model")
	}
}

func TestListFailsOnABrokenIndex(t *testing.T) {
	c, _ := testCatalog(t)
	touch(t, c.Index.Path)
	if err := os.WriteFile(c.Index.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.List(); err == nil {
		t.Fatal("broken index must fail the list")
	}
}

func TestFindFailsOnABrokenIndex(t *testing.T) {
	c, _ := testCatalog(t)
	touch(t, c.Index.Path)
	if err := os.WriteFile(c.Index.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Find("mine"); err == nil || ok {
		t.Fatalf("find over a broken index must fail: ok=%v err=%v", ok, err)
	}
}

func TestListReportsAProjectorWithoutItsModel(t *testing.T) {
	c, store := testCatalog(t)
	gemma, _ := c.Registry.Find("gemma4:12b")
	touch(t, store.Path(gemma.HFRepo, gemma.MMProj.File))
	it, ok, err := c.Find("gemma4:12b")
	if err != nil || !ok {
		t.Fatalf("find gemma: %v %v", ok, err)
	}
	if it.MMProjPath != store.Path(gemma.HFRepo, gemma.MMProj.File) || it.Pulled {
		t.Fatalf("projector or pulled flag wrong: %+v", it)
	}
}

// writable puts a file with the given mode under dir and returns its path.
func writable(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("gguf"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// skipAsRoot skips a test that needs the permission checks of a user who is not root.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
}

// The paths below are what an index entry may name that the catalog cannot take for a model. Each makes the path
// inside dir and returns it.

func goneFile(_ *testing.T, dir string) string { return filepath.Join(dir, "gone.gguf") }

func fileBelowAFile(t *testing.T, dir string) string {
	return filepath.Join(writable(t, dir, "plain", 0o600), "model.gguf")
}

func directoryNamedAsAModel(t *testing.T, dir string) string {
	path := filepath.Join(dir, "a-directory.gguf")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileThatCannotBeRead(t *testing.T, dir string) string {
	path := writable(t, dir, "locked.gguf", 0o600)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	return path
}

func fileInADirectoryThatCannotBeSearched(t *testing.T, dir string) string {
	inner := filepath.Join(dir, "closed")
	if err := os.Mkdir(inner, 0o750); err != nil {
		t.Fatal(err)
	}
	path := writable(t, inner, "model.gguf", 0o600)
	if err := os.Chmod(inner, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(inner, 0o750) })
	return path
}

// assertListed checks that the list is whole although one import has a file that cannot be examined: it is there
// next to the registry's items, none of which got a problem from it.
func assertListed(t *testing.T, c Catalog) {
	t.Helper()
	items, err := c.List()
	if err != nil {
		t.Fatalf("one file that cannot be examined must not fail the list: %v", err)
	}
	if len(items) != len(c.Registry.Entries)+2 {
		t.Fatalf("%d items, want the registry's and the two imports", len(items))
	}
	for _, it := range items[:len(c.Registry.Entries)] {
		if it.Problem != "" {
			t.Fatalf("a catalog item got a problem from an import: %+v", it)
		}
	}
}

// assertBad checks the item of the import whose file cannot be examined: not pulled, still its own, and with the
// problem recorded when there is one to record.
func assertBad(t *testing.T, got Item, bad, problem string) {
	t.Helper()
	if got.Pulled || got.ModelPath != bad || !got.Local || !got.Inferred || got.Size != 4 {
		t.Fatalf("the item of the file that cannot be examined: %+v", got)
	}
	if (problem == "") != (got.Problem == "") || !strings.Contains(got.Problem, problem) {
		t.Fatalf("problem %q, want one containing %q", got.Problem, problem)
	}
	if problem != "" && !strings.Contains(got.Problem, bad) {
		t.Fatalf("problem %q does not name the file %s", got.Problem, bad)
	}
}

// assertKept checks the answer of the catalog about a model whose file is bad, next to one whose file is good.
func assertKept(t *testing.T, c Catalog, good, bad, problem string) {
	t.Helper()
	assertListed(t, c)
	got, ok, err := c.Find("bad")
	if err != nil || !ok {
		t.Fatalf("find bad: %v %v", ok, err)
	}
	assertBad(t, got, bad, problem)
	healthy, ok, err := c.Find("good")
	if err != nil || !ok || !healthy.Pulled || healthy.Problem != "" || healthy.ModelPath != good {
		t.Fatalf("the other import was touched: %+v %v %v", healthy, ok, err)
	}
}

func TestListKeepsAnItemWhoseFileCannotBeExamined(t *testing.T) {
	cases := []struct {
		name    string
		path    func(t *testing.T, dir string) string
		problem string // what the item's Problem has to contain; "" when the file is simply gone
		perms   bool   // the case relies on permissions, which root does not have
	}{
		{"a file that is gone", goneFile, "", false},
		{"a path below a regular file", fileBelowAFile, "not a directory", false},
		{"a directory", directoryNamedAsAModel, "is not a regular file", false},
		{"a file that cannot be read", fileThatCannotBeRead, "permission denied", true},
		{"a directory that cannot be searched", fileInADirectoryThatCannotBeSearched, "permission denied", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.perms {
				skipAsRoot(t)
			}
			c, _ := testCatalog(t)
			dir := t.TempDir()
			good, bad := writable(t, dir, "good.gguf", 0o600), tc.path(t, dir)
			for name, path := range map[string]string{"good": good, "bad": bad} {
				if err := c.Index.Add(IndexEntry{Name: name, Path: path, Size: 4}); err != nil {
					t.Fatal(err)
				}
			}
			assertKept(t, c, good, bad, tc.problem)
		})
	}
}
