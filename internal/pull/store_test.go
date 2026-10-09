package pull

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorePathsAndExists(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	p := s.Path("ggml-org/models-moved", "tinyllamas/stories260K.gguf")
	if p != filepath.Join(s.Dir, "ggml-org", "models-moved", "tinyllamas", "stories260K.gguf") {
		t.Fatalf("path %s", p)
	}
	if s.Exists("ggml-org/models-moved", "tinyllamas/stories260K.gguf") {
		t.Fatal("exists before any file")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+".part", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.Exists("ggml-org/models-moved", "tinyllamas/stories260K.gguf") {
		t.Fatal("a part file must not count")
	}
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !s.Exists("ggml-org/models-moved", "tinyllamas/stories260K.gguf") {
		t.Fatal("exists after the file is in place")
	}
}

func TestStoreRemovePrunesEmptyDirectories(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	p := s.Path("org/repo", "a.gguf")
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{p, p + ".part", s.Path("org/repo", "b.gguf")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Remove("org/repo", "a.gguf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(p)); err != nil {
		t.Fatal("the repo directory still holds b.gguf and must stay")
	}
	if err := s.Remove("org/repo", "b.gguf", "absent.gguf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "org")); !os.IsNotExist(err) {
		t.Fatal("empty owner directory must be pruned")
	}
	if _, err := os.Stat(s.Dir); err != nil {
		t.Fatal("the store root must never be removed")
	}
}

func TestStoreRemoveReportsAStuckPath(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	p := s.Path("org/repo", "a.gguf")
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "inside"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("org/repo", "a.gguf"); err == nil {
		t.Fatal("a non-empty directory where a blob belongs must be reported")
	}
}

func TestStoreLinkLocal(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	src := filepath.Join(t.TempDir(), "My-Model.gguf")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link, err := s.LinkLocal("My-Model", src)
	if err != nil {
		t.Fatal(err)
	}
	if target, _ := os.Readlink(link); target != src {
		t.Fatalf("link points to %s", target)
	}
	if _, err := s.LinkLocal("x", "relative.gguf"); err == nil {
		t.Fatal("relative path accepted")
	}
	if _, err := s.LinkLocal("x", filepath.Join(t.TempDir(), "absent.gguf")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestStoreLinkLocalReportsBrokenStores(t *testing.T) {
	src := filepath.Join(t.TempDir(), "x.gguf")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := Store{Dir: filepath.Join(t.TempDir(), "file")}
	if err := os.WriteFile(broken.Dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := broken.LinkLocal("x", src); err == nil {
		t.Fatal("a file where the store root should be must fail")
	}
	s := Store{Dir: t.TempDir()}
	link := filepath.Join(s.Dir, "local", "x.gguf")
	if err := os.MkdirAll(link, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(link, "inside"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkLocal("x", src); err == nil {
		t.Fatal("a directory where the link belongs must fail")
	}
}

func TestStoreRefusesNamesThatLeaveTheStore(t *testing.T) {
	root := t.TempDir()
	s := Store{Dir: filepath.Join(root, "store")}
	outside := filepath.Join(root, "outside.gguf")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Each pair resolves to root/outside.gguf, one level above the store.
	pairs := []struct{ repo, file string }{{"..", "outside.gguf"}, {"org/../..", "outside.gguf"}, {"org", "../../outside.gguf"}}
	for _, p := range pairs {
		if err := s.Remove(p.repo, p.file); err == nil {
			t.Fatalf("Remove(%q, %q) must refuse a name that leaves the store", p.repo, p.file)
		}
		if s.Exists(p.repo, p.file) {
			t.Fatalf("Exists(%q, %q) must not see a file outside the store", p.repo, p.file)
		}
	}
	if got, _ := os.ReadFile(outside); string(got) != "keep" {
		t.Fatal("a refused name changed a file outside the store")
	}
	src := filepath.Join(root, "src.gguf")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkLocal("../../escape", src); err == nil {
		t.Fatal("LinkLocal must refuse a name that leaves the local links")
	}
	if _, err := os.Lstat(filepath.Join(root, "escape.gguf")); !os.IsNotExist(err) {
		t.Fatal("a refused link name still created a link outside the store")
	}
}

func TestStoreLinkLocalReplacesAnOldLink(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	first := filepath.Join(t.TempDir(), "first.gguf")
	second := filepath.Join(t.TempDir(), "second.gguf")
	for _, f := range []string{first, second} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link, err := s.LinkLocal("M", first)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.LinkLocal("M", second)
	if err != nil {
		t.Fatalf("a symlink from an earlier pull must be replaced: %v", err)
	}
	if again != link {
		t.Fatalf("the link path moved from %s to %s", link, again)
	}
	if target, _ := os.Readlink(link); target != second {
		t.Fatalf("link points to %s, want %s", target, second)
	}
}

func TestStoreLinkLocalRefusesADirectory(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	dir := t.TempDir()
	if _, err := s.LinkLocal("x", dir); err == nil {
		t.Fatal("a directory is not a model file")
	}
	if _, err := os.Lstat(filepath.Join(s.Dir, "local", "x.gguf")); !os.IsNotExist(err) {
		t.Fatal("no link may be made for a directory")
	}
}

func TestStoreLinkLocalKeepsARegularFile(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	src := filepath.Join(t.TempDir(), "src.gguf")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(s.Dir, "local", "x.gguf")
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkLocal("x", src); err == nil {
		t.Fatal("a regular file at the link path must be an error")
	}
	if got, _ := os.ReadFile(link); string(got) != "keep" {
		t.Fatal("LinkLocal changed a regular file it was told to leave alone")
	}
}

func TestStoreLinkLocalStopsOnAPathItCannotInspect(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	src := filepath.Join(t.TempDir(), "src.gguf")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// "sub" is a regular file, so the link sub/x cannot be inspected and the error must surface.
	blocker := filepath.Join(s.Dir, "local", "sub")
	if err := os.MkdirAll(filepath.Dir(blocker), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LinkLocal("sub/x", src); err == nil {
		t.Fatal("a link path under a file must be an error")
	}
}

func TestFreeSpaceOfTheTempDir(t *testing.T) {
	n, err := FreeSpace(t.TempDir())
	if err != nil || n <= 0 {
		t.Fatalf("free space %d, %v", n, err)
	}
	if _, err := FreeSpace(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing dir must fail")
	}
}
