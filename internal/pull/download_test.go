package pull

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testBody(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(7)).Read(b)
	return b
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// blobServer serves body at /<repo>/resolve/main/<file>, honoring Range unless ignoreRange is set.
func blobServer(t *testing.T, body []byte, ignoreRange bool) (*httptest.Server, *[]string) {
	t.Helper()
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		ranges = append(ranges, rng)
		if rng == "" || ignoreRange {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
			return
		}
		var start int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err != nil || start >= int64(len(body)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[start:])
	}))
	t.Cleanup(srv.Close)
	return srv, &ranges
}

func newTestDownloader(url string) (*Downloader, *[]Progress) {
	var seen []Progress
	d := NewDownloader("tok")
	d.BaseURL = url
	d.FreeSpace = func(string) (int64, error) { return 1 << 40, nil }
	d.OnProgress = func(p Progress) { seen = append(seen, p) }
	return d, &seen
}

func TestFetchDownloadsAndVerifies(t *testing.T) {
	body := testBody(3 << 20)
	srv, ranges := blobServer(t, body, false)
	d, seen := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "org", "repo", "m.gguf")
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: int64(len(body)), SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Fatal("content differs")
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal("part file left behind")
	}
	if (*ranges)[0] != "" {
		t.Fatalf("a fresh download sent Range %q", (*ranges)[0])
	}
	last := (*seen)[len(*seen)-1]
	if last.Stage != "done" || last.Bytes != art.Size {
		t.Fatalf("last progress %+v", last)
	}
	stages := map[string]bool{}
	for _, p := range *seen {
		stages[p.Stage] = true
	}
	if !stages["downloading"] || !stages["verifying"] {
		t.Fatalf("stages seen: %v", stages)
	}
}

func TestFetchResumesAfterInterruption(t *testing.T) {
	body := testBody(2 << 20)
	srv, ranges := blobServer(t, body, false)
	d, seen := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(dest+".part", body[:1000], 0o600); err != nil {
		t.Fatal(err)
	}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: int64(len(body)), SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if (*ranges)[0] != "bytes=1000-" {
		t.Fatalf("Range %q", (*ranges)[0])
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Fatal("resumed content differs")
	}
	if (*seen)[0].Bytes < 1000 {
		t.Fatalf("progress did not start from the resumed size: %+v", (*seen)[0])
	}
}

func TestFetchRestartsWhenServerIgnoresRange(t *testing.T) {
	body := testBody(1 << 20)
	srv, _ := blobServer(t, body, true)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(dest+".part", []byte(strings.Repeat("x", 1000)), 0o600); err != nil {
		t.Fatal(err)
	}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: int64(len(body)), SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Fatal("a 200 answer to a Range request must restart the file")
	}
}

func TestFetchDiscardsAStalePart(t *testing.T) {
	body := testBody(1000)
	srv, ranges := blobServer(t, body, false)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(dest+".part", testBody(5000), 0o600); err != nil {
		t.Fatal(err)
	}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 1000, SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if (*ranges)[0] != "" {
		t.Fatalf("a part bigger than the artifact must not be resumed, Range %q", (*ranges)[0])
	}
}

func TestFetchRejectsAChecksumMismatch(t *testing.T) {
	body := testBody(1000)
	srv, _ := blobServer(t, body, false)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 1000, SHA256: strings.Repeat("0", 64)}
	err := d.Fetch(context.Background(), art, dest)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	for _, p := range []string{dest, dest + ".part"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s must not exist after a mismatch", p)
		}
	}
}

func TestFetchChecksFreeSpaceBeforeAnyRequest(t *testing.T) {
	srv, ranges := blobServer(t, testBody(10), false)
	d, _ := newTestDownloader(srv.URL)
	d.FreeSpace = func(string) (int64, error) { return 10, nil }
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 3 << 20, SHA256: strings.Repeat("0", 64)}
	err := d.Fetch(context.Background(), art, filepath.Join(t.TempDir(), "m.gguf"))
	if err == nil || !strings.Contains(err.Error(), "free") {
		t.Fatalf("want a free-space error, got %v", err)
	}
	if len(*ranges) != 0 {
		t.Fatal("a request was sent although the disk is full")
	}
}

func TestFetchServerErrorAndToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	d, _ := newTestDownloader(srv.URL)
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 10, SHA256: strings.Repeat("0", 64)}
	err := d.Fetch(context.Background(), art, filepath.Join(t.TempDir(), "m.gguf"))
	if err == nil || !strings.Contains(err.Error(), "answered 500") {
		t.Fatalf("want answered 500, got %v", err)
	}
	if auth != "Bearer tok" {
		t.Fatalf("token not sent: %q", auth)
	}
}

func TestFetchStopsOnCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			_, _ = w.Write(make([]byte, 10000))
			f.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	t.Cleanup(srv.Close)
	d, _ := newTestDownloader(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(60 * time.Millisecond); cancel() }()
	dest := filepath.Join(t.TempDir(), "m.gguf")
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 1000000, SHA256: strings.Repeat("0", 64)}
	if err := d.Fetch(ctx, art, dest); err == nil {
		t.Fatal("a cancelled fetch must fail")
	}
	info, err := os.Stat(dest + ".part")
	if err != nil || info.Size() == 0 {
		t.Fatal("the part file must stay for a later resume")
	}
}

func TestFetchEscapesEachPathSegment(t *testing.T) {
	body := testBody(1000)
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.EscapedPath()
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	d, _ := newTestDownloader(srv.URL)
	art := Artifact{Repo: "org/repo", File: "dir/odd name#1.gguf", Size: int64(len(body)), SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, filepath.Join(t.TempDir(), "m.gguf")); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := "/org/repo/resolve/main/dir/odd%20name%231.gguf"; asked != want {
		t.Fatalf("requested %s, want %s", asked, want)
	}
}

func TestFetchVerifiesACompletePart(t *testing.T) {
	body := testBody(4096)
	srv, ranges := blobServer(t, body, false)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(dest+".part", body, 0o600); err != nil {
		t.Fatal(err)
	}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: int64(len(body)), SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(*ranges) != 0 {
		t.Fatal("a complete part must be verified without a request")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("the verified part must be renamed into place")
	}
}

func TestFetchIgnoresAFailingSpaceProbe(t *testing.T) {
	body := testBody(1000)
	srv, _ := blobServer(t, body, false)
	d, _ := newTestDownloader(srv.URL)
	d.FreeSpace = func(string) (int64, error) { return 0, errors.New("statfs failed") }
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 1000, SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, filepath.Join(t.TempDir(), "m.gguf")); err != nil {
		t.Fatalf("an unknown filesystem must not block a pull: %v", err)
	}
}

func TestFetchWithoutProbes(t *testing.T) {
	body := testBody(1000)
	srv, _ := blobServer(t, body, false)
	d := NewDownloader("")
	d.BaseURL = srv.URL
	d.FreeSpace = nil
	d.OnProgress = nil
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 1000, SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, filepath.Join(t.TempDir(), "m.gguf")); err != nil {
		t.Fatalf("fetch: %v", err)
	}
}

func TestFetchRejectsABadBaseURL(t *testing.T) {
	d, _ := newTestDownloader("http://[::1")
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 10, SHA256: strings.Repeat("0", 64)}
	if err := d.Fetch(context.Background(), art, filepath.Join(t.TempDir(), "m.gguf")); err == nil {
		t.Fatal("a malformed base URL must fail before any connection")
	}
}

func TestFetchReportsAnUnreachableServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()
	d, _ := newTestDownloader(base)
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 10, SHA256: strings.Repeat("0", 64)}
	if err := d.Fetch(context.Background(), art, filepath.Join(t.TempDir(), "m.gguf")); err == nil {
		t.Fatal("a refused connection must fail")
	}
}

func TestFetchKeepsThePartOfACutConnection(t *testing.T) {
	body := testBody(1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body[:500])
	}))
	t.Cleanup(srv.Close)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 1000, SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, dest); err == nil {
		t.Fatal("a cut download must fail")
	}
	info, err := os.Stat(dest + ".part")
	if err != nil || info.Size() != 500 {
		t.Fatalf("the part must keep the 500 bytes that arrived: %v", err)
	}
}

func TestFetchRefusesAPartThatIsADirectory(t *testing.T) {
	body := testBody(1 << 20)
	srv, _ := blobServer(t, body, false)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.Mkdir(dest+".part", 0o750); err != nil {
		t.Fatal(err)
	}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: int64(len(body)), SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, dest); err == nil {
		t.Fatal("a directory in the way of the part must fail")
	}
}

func TestFetchReportsAPartThatCannotBeRead(t *testing.T) {
	srv, ranges := blobServer(t, testBody(10), false)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.Mkdir(dest+".part", 0o750); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest + ".part")
	if err != nil {
		t.Fatal(err)
	}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: info.Size(), SHA256: strings.Repeat("0", 64)}
	if err := d.Fetch(context.Background(), art, dest); err == nil {
		t.Fatal("a part that cannot be read must fail the verification")
	}
	if len(*ranges) != 0 {
		t.Fatal("a complete part must not be downloaded again")
	}
}

func TestFetchFailsWhenDestIsADirectory(t *testing.T) {
	body := testBody(1000)
	srv, _ := blobServer(t, body, false)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.Mkdir(dest, 0o750); err != nil {
		t.Fatal(err)
	}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 1000, SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, dest); err == nil {
		t.Fatal("a directory at the destination must fail the rename")
	}
}

func TestFetchFailsWhenTheDirectoryCannotBeMade(t *testing.T) {
	srv, ranges := blobServer(t, testBody(10), false)
	d, _ := newTestDownloader(srv.URL)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 10, SHA256: strings.Repeat("0", 64)}
	if err := d.Fetch(context.Background(), art, filepath.Join(blocker, "m.gguf")); err == nil {
		t.Fatal("a file where the directory belongs must fail")
	}
	if len(*ranges) != 0 {
		t.Fatal("no request may be sent when the directory cannot be made")
	}
}

func TestFetchReportsAFailingWrite(t *testing.T) {
	d := &Downloader{}
	err := d.copy(context.Background(), failingWriter{}, strings.NewReader("abc"), 0, 3)
	if err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("want a write error, got %v", err)
	}
}

func TestFetchCopyChecksTheContextFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := &Downloader{}
	err := d.copy(ctx, io.Discard, strings.NewReader("abc"), 0, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestFetchKeepsThePartOfAShortChunkedBody(t *testing.T) {
	body := testBody(2000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body[:1000])
		w.(http.Flusher).Flush() // no Content-Length: the body is chunked and ends cleanly after 1000 bytes
	}))
	t.Cleanup(srv.Close)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 2000, SHA256: sumOf(body)}
	err := d.Fetch(context.Background(), art, dest)
	if err == nil || !strings.Contains(err.Error(), "ended short at 1000 of 2000 bytes") {
		t.Fatalf("want the short-body error, got %v", err)
	}
	info, statErr := os.Stat(dest + ".part")
	if statErr != nil || info.Size() != 1000 {
		t.Fatal("the part must keep the 1000 bytes that arrived")
	}
}

func TestFetchLeavesThePartAloneOnAWrongLengthAnswer(t *testing.T) {
	body := testBody(4096)
	page := []byte("<html>blocked by proxy</html>")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(page)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(page)
	}))
	t.Cleanup(srv.Close)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	part := body[:1000]
	if err := os.WriteFile(dest+".part", part, 0o600); err != nil {
		t.Fatal(err)
	}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: int64(len(body)), SHA256: sumOf(body)}
	want := fmt.Sprintf("the server sent %d bytes where %d were expected", len(page), len(body))
	err := d.Fetch(context.Background(), art, dest)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got %v", want, err)
	}
	got, _ := os.ReadFile(dest + ".part")
	if !bytes.Equal(got, part) {
		t.Fatal("a bogus 200 answer touched the good part")
	}
}

func TestFetchCapsAChunkedBodyAtTheArtifactSize(t *testing.T) {
	body := testBody(1000)
	long := append(bytes.Clone(body), []byte("trailing bytes that are not part of the file")...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(long)
		w.(http.Flusher).Flush() // chunked, so the length is not announced
	}))
	t.Cleanup(srv.Close)
	d, _ := newTestDownloader(srv.URL)
	dest := filepath.Join(t.TempDir(), "m.gguf")
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 1000, SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Fatal("bytes past the artifact size reached the file")
	}
}

func TestFetchChecksOnlyTheBytesLeft(t *testing.T) {
	body := testBody(4096)
	const have = 1000
	srv, ranges := blobServer(t, body, false)
	d, _ := newTestDownloader(srv.URL)
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: int64(len(body)), SHA256: sumOf(body)}

	// Exactly the bytes still missing fit: the space check counts what is left, not the whole file.
	fits := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(fits+".part", body[:have], 0o600); err != nil {
		t.Fatal(err)
	}
	d.FreeSpace = func(string) (int64, error) { return int64(len(body) - have), nil }
	if err := d.Fetch(context.Background(), art, fits); err != nil {
		t.Fatalf("exactly the bytes left must fit: %v", err)
	}

	// One byte less fails before any request is sent.
	short := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(short+".part", body[:have], 0o600); err != nil {
		t.Fatal(err)
	}
	d.FreeSpace = func(string) (int64, error) { return int64(len(body) - have - 1), nil }
	before := len(*ranges)
	if err := d.Fetch(context.Background(), art, short); err == nil || !strings.Contains(err.Error(), "free") {
		t.Fatalf("one byte short must fail the space check, got %v", err)
	}
	if len(*ranges) != before {
		t.Fatal("a request was sent although the space check failed")
	}
}

func TestFetchRejectsAnArtifactWithoutASize(t *testing.T) {
	srv, ranges := blobServer(t, testBody(10), false)
	d, _ := newTestDownloader(srv.URL)
	for _, size := range []int64{0, -1} {
		art := Artifact{Repo: "org/repo", File: "m.gguf", Size: size, SHA256: strings.Repeat("0", 64)}
		err := d.Fetch(context.Background(), art, filepath.Join(t.TempDir(), "m.gguf"))
		if err == nil || !strings.Contains(err.Error(), "cannot be verified") {
			t.Fatalf("size %d: want a clear refusal, got %v", size, err)
		}
	}
	if len(*ranges) != 0 {
		t.Fatal("no request may be sent for an artifact without a size")
	}
}

func TestFetchWorksFromALiteralDownloader(t *testing.T) {
	body := testBody(1000)
	srv, _ := blobServer(t, body, false)
	d := &Downloader{BaseURL: srv.URL}
	art := Artifact{Repo: "org/repo", File: "m.gguf", Size: 1000, SHA256: sumOf(body)}
	if err := d.Fetch(context.Background(), art, filepath.Join(t.TempDir(), "m.gguf")); err != nil {
		t.Fatalf("a literal Downloader must fall back to a default client: %v", err)
	}
}

// failingWriter stands in for a disk that is full.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
