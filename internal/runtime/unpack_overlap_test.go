package runtime

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile puts a file under dir, the way an earlier unpack into the same staging directory would have.
func writeFile(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUnpackAcceptsAFileThatAlreadyHoldsTheSameBytes(t *testing.T) {
	dest := t.TempDir()
	path := writeFile(t, dest, "LICENSE", "MIT", 0o600)
	tgz := makeTarGz(t, []tarEntry{
		{name: "cudart-b/LICENSE", typ: tar.TypeReg, body: "MIT"},
		{name: "cudart-b/libcudart.so.12", typ: tar.TypeReg, body: "lib"},
	})
	if err := Unpack(bytes.NewReader(tgz), dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the file that was already there was rewritten: %v %v", info, err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "libcudart.so.12")); err != nil || string(got) != "lib" {
		t.Fatalf("the new file %q, %v", got, err)
	}
}

func TestUnpackCountsAnAcceptedOverlapAsTheArchivesFile(t *testing.T) {
	dest := t.TempDir()
	writeFile(t, dest, "LICENSE", "MIT", 0o600)
	tgz := makeTarGz(t, []tarEntry{{name: "cudart-b/LICENSE", typ: tar.TypeReg, body: "MIT"}})
	if err := Unpack(bytes.NewReader(tgz), dest); err != nil {
		t.Fatalf("an archive of one accepted overlap was refused: %v", err)
	}
}

func TestUnpackRefusesToReplaceAFileWithOtherBytes(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{"same size", "MIT", "XYZ"},
		{"longer", "MIT", "MIT and more"},
		{"shorter", "MIT and more", "MIT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			path := writeFile(t, dest, "LICENSE", tc.old, 0o600)
			tgz := makeTarGz(t, []tarEntry{
				{name: "cudart-b/good", typ: tar.TypeReg, body: "x"},
				{name: "cudart-b/LICENSE", typ: tar.TypeReg, body: tc.new},
			})
			err := Unpack(bytes.NewReader(tgz), dest)
			if err == nil || !strings.Contains(err.Error(), "refusing to replace") || !strings.Contains(err.Error(), path) {
				t.Fatalf("want the replacement refused by name, got %v", err)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.old {
				t.Fatalf("the file holds %q after a refused replacement, want %q", got, tc.old)
			}
			if entries, _ := os.ReadDir(dest); len(entries) != 1 {
				t.Fatalf("the refused unpack left %v behind", entries)
			}
		})
	}
}

func TestUnpackRefusesToReplaceWhatIsNotAFile(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dest string)
		entry tarEntry
	}{
		{"a file over a link", func(t *testing.T, dest string) {
			if err := os.Symlink("target", filepath.Join(dest, "name")); err != nil {
				t.Fatal(err)
			}
		}, tarEntry{name: "cudart-b/name", typ: tar.TypeReg, body: "target"}},
		{"a file over a directory", func(t *testing.T, dest string) {
			if err := os.Mkdir(filepath.Join(dest, "name"), 0o750); err != nil {
				t.Fatal(err)
			}
		}, tarEntry{name: "cudart-b/name", typ: tar.TypeReg, body: ""}},
		{"a link over a file", func(t *testing.T, dest string) {
			writeFile(t, dest, "name", "target", 0o600)
		}, tarEntry{name: "cudart-b/name", typ: tar.TypeSymlink, link: "target"}},
		{"a link over another link", func(t *testing.T, dest string) {
			if err := os.Symlink("other", filepath.Join(dest, "name")); err != nil {
				t.Fatal(err)
			}
		}, tarEntry{name: "cudart-b/name", typ: tar.TypeSymlink, link: "target"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			tc.setup(t, dest)
			tgz := makeTarGz(t, []tarEntry{{name: "cudart-b/good", typ: tar.TypeReg, body: "x"}, tc.entry})
			err := Unpack(bytes.NewReader(tgz), dest)
			if err == nil || !strings.Contains(err.Error(), "refusing to replace") {
				t.Fatalf("want the replacement refused, got %v", err)
			}
			if entries, _ := os.ReadDir(dest); len(entries) != 1 {
				t.Fatalf("the refused unpack left %v behind", entries)
			}
		})
	}
}

func TestUnpackAcceptsALinkThatAlreadyPointsWhereTheEntryPoints(t *testing.T) {
	dest := t.TempDir()
	if err := os.Symlink("libx.so.1", filepath.Join(dest, "libx.so")); err != nil {
		t.Fatal(err)
	}
	tgz := makeTarGz(t, []tarEntry{
		{name: "cudart-b/libx.so", typ: tar.TypeSymlink, link: "libx.so.1"},
		{name: "cudart-b/libx.so.1", typ: tar.TypeReg, body: "lib"},
	})
	if err := Unpack(bytes.NewReader(tgz), dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(dest, "libx.so")); err != nil || target != "libx.so.1" {
		t.Fatalf("link %q, %v", target, err)
	}
}

func TestUnpackFailsWhenTheEntryOfAnOverlapCannotBeRead(t *testing.T) {
	body := make([]byte, 64<<10)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	writeFile(t, dest, "big", string(body), 0o600)
	full := makeTarGz(t, []tarEntry{{name: "cudart-b/big", typ: tar.TypeReg, body: string(body)}})
	if err := Unpack(bytes.NewReader(full[:len(full)/2]), dest); err == nil {
		t.Fatal("a truncated entry that overlaps a file was accepted")
	}
	if got, _ := os.ReadFile(filepath.Join(dest, "big")); !bytes.Equal(got, body) {
		t.Fatal("the file changed")
	}
}

func TestUnpackFailsWhenTheFileOfAnOverlapCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	dest := t.TempDir()
	path := writeFile(t, dest, "LICENSE", "MIT", 0o600)
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	tgz := makeTarGz(t, []tarEntry{{name: "cudart-b/LICENSE", typ: tar.TypeReg, body: "MIT"}})
	if err := Unpack(bytes.NewReader(tgz), dest); err == nil {
		t.Fatal("an unreadable file was taken for the same bytes")
	}
}
