package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/pull"
)

// afterBody serves a body and runs act when the last byte has been read, which is the moment a download ends and
// the next step opens the file. It stands for a disk or another process that changes the file in between.
type afterBody struct {
	r   io.Reader
	act func()
}

func (b *afterBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		b.act()
	}
	return n, err
}

func (b *afterBody) Close() error { return nil }

// changeAfterDownload is a client whose answer is body, and which runs act once the body has been read.
func changeAfterDownload(body []byte, act func()) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{}, ContentLength: -1, Request: r,
			Body: &afterBody{r: bytes.NewReader(body), act: act},
		}, nil
	})}
}

func TestInstallReportsAnArchiveThatVanishesAfterItsDownload(t *testing.T) {
	_, lock, p := darwinInstaller(t)
	dir := t.TempDir()
	archive := filepath.Join(dir, lock.Assets["darwin/arm64"].Asset+".download")
	inst := &Installer{Dir: dir, BaseURL: "http://release.invalid", Client: changeAfterDownload([]byte("x"), func() {
		_ = os.Remove(archive)
	})}
	if _, err := inst.Install(context.Background(), lock, p, false); err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("want the open failure, got %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a failed install left %v behind", entries)
	}
}

func TestInstallReportsAnArchiveThatCannotBeRead(t *testing.T) {
	_, lock, p := darwinInstaller(t)
	dir := t.TempDir()
	archive := filepath.Join(dir, lock.Assets["darwin/arm64"].Asset+".download")
	inst := &Installer{Dir: dir, BaseURL: "http://release.invalid", Client: changeAfterDownload([]byte("x"), func() {
		_ = os.Remove(archive)
		_ = os.Mkdir(archive, 0o750) // opens, and fails to read
	})}
	if _, err := inst.Install(context.Background(), lock, p, false); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("want the read failure, got %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a failed install left %v behind", entries)
	}
}

// failingWriter accepts n bytes and then fails, like a disk that fills up.
type failingWriter struct{ n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, errors.New("no space left on device")
	}
	w.n -= len(p)
	return len(p), nil
}

func TestCopyBodyReportsAWriteFailure(t *testing.T) {
	var stages []pull.Progress
	inst := &Installer{OnProgress: func(p pull.Progress) { stages = append(stages, p) }}
	body := strings.NewReader(strings.Repeat("x", 3<<20)) // three blocks: the second write fails
	err := inst.copyBody(&failingWriter{n: 1 << 20}, body, 3<<20, "http://release.invalid/asset")
	if err == nil || !strings.Contains(err.Error(), "no space left") {
		t.Fatalf("want the write failure, got %v", err)
	}
	if len(stages) != 1 || stages[0].Bytes != 1<<20 || stages[0].Total != 3<<20 {
		t.Fatalf("progress before the failure: %+v", stages)
	}
}

func TestCopyBodyReportsAReadFailure(t *testing.T) {
	inst := &Installer{}
	err := inst.copyBody(io.Discard, io.MultiReader(strings.NewReader("abc"), errReader{}), -1, "http://release.invalid/asset")
	if err == nil || !strings.Contains(err.Error(), "connection reset") || !strings.Contains(err.Error(), "release.invalid") {
		t.Fatalf("want the read failure with the url, got %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }
