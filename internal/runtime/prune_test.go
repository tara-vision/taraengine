package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// plant creates the entries under dir: a name ending in a slash is a directory, any other a file.
func plant(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if name[len(name)-1] == '/' {
			if err := os.MkdirAll(path, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "llama-server"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveOtherBuildsKeepsThePinnedOneAndWhatIsNoBuild(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, "b11514/", "b11514.partial/", "b11514.old/", "b10000/", "b10000.old/", "b9999.partial/",
		"notes.txt", "custom/", "bogus-b1/")
	removed, err := RemoveOtherBuilds(dir, "b11514")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(removed)
	if want := []string{"b10000", "b10000.old", "b9999.partial"}; !slices.Equal(removed, want) {
		t.Fatalf("removed %v, want %v", removed, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{"b11514", "b11514.old", "b11514.partial", "bogus-b1", "custom", "notes.txt"}; !slices.Equal(left, want) {
		t.Fatalf("left %v, want %v", left, want)
	}
}

func TestRemoveOtherBuildsOfAMissingDirectoryRemovesNothing(t *testing.T) {
	removed, err := RemoveOtherBuilds(filepath.Join(t.TempDir(), "absent"), "b11514")
	if err != nil || len(removed) != 0 {
		t.Fatalf("got %v %v", removed, err)
	}
}

func TestRemoveOtherBuildsReportsWhatItCannotRemove(t *testing.T) {
	dir := t.TempDir()
	plant(t, dir, "b10000/")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	removed, err := RemoveOtherBuilds(dir, "b11514")
	if err == nil || len(removed) != 0 || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("got %v %v", removed, err)
	}
}

func TestRemoveOtherBuildsReportsADirectoryItCannotRead(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	plant(t, filepath.Dir(file), "a-file")
	if _, err := RemoveOtherBuilds(file, "b11514"); err == nil {
		t.Fatal("a runtime directory that is a file cannot be read")
	}
}
