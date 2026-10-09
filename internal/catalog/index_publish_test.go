package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// recordingFile is a temp file that notes the order in which it is written, synced and closed.
type recordingFile struct {
	*os.File
	calls    *[]string
	syncFail error
}

func (f recordingFile) Write(p []byte) (int, error) {
	*f.calls = append(*f.calls, "write")
	return f.File.Write(p)
}

func (f recordingFile) Sync() error {
	*f.calls = append(*f.calls, "sync")
	if f.syncFail != nil {
		return f.syncFail
	}
	return f.File.Sync()
}

func (f recordingFile) Close() error {
	*f.calls = append(*f.calls, "close")
	return f.File.Close()
}

func tempIn(t *testing.T, dir string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(dir, "index-*.json.tmp")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPublishSyncsTheTempFileBeforeItIsRenamedIntoPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	var calls []string
	if err := publish(recordingFile{File: tempIn(t, dir), calls: &calls}, []byte("[]"), path); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !slices.Equal(calls, []string{"write", "sync", "close"}) {
		t.Fatalf("the file was used in the order %v, want write, sync, close", calls)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "[]" {
		t.Fatalf("published %q, %v", data, err)
	}
}

func TestPublishLeavesTheOldFileWhenTheSyncFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	tmp := recordingFile{File: tempIn(t, dir), calls: &calls, syncFail: errors.New("input/output error")}
	if err := publish(tmp, []byte("[]"), path); err == nil {
		t.Fatal("a failed sync must fail the publish")
	}
	if !slices.Equal(calls, []string{"write", "sync", "close"}) {
		t.Fatalf("the file was used in the order %v, want it closed after the failed sync", calls)
	}
	if data, _ := os.ReadFile(path); string(data) != "old" {
		t.Fatalf("the index holds %q after a failed publish, want the old one", data)
	}
}

func TestPublishFailsWhenTheTempFileCannotBeWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	tmp := tempIn(t, dir)
	if err := tmp.Close(); err != nil { // a closed file takes no write
		t.Fatal(err)
	}
	if err := publish(tmp, []byte("[]"), path); err == nil {
		t.Fatal("writing to a closed file must fail the publish")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a failed publish created the index")
	}
}
