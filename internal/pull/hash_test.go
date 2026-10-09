package pull

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSHA256File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abc.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	if sum != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("sum %s", sum)
	}
	if _, err := SHA256File(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing file must fail")
	}
}

func TestSHA256FileOfADirectory(t *testing.T) {
	if _, err := SHA256File(t.TempDir()); err == nil {
		t.Fatal("a directory cannot be hashed")
	}
}

func TestSHA256Reader(t *testing.T) {
	sum, err := SHA256Reader(strings.NewReader("abc"))
	if err != nil || sum != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatalf("sum %s, %v", sum, err)
	}
	failing := io.MultiReader(strings.NewReader("ab"), readFailure{})
	if _, err := SHA256Reader(failing); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("want the read failure, got %v", err)
	}
}

type readFailure struct{}

func (readFailure) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
