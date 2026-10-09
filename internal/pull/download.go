package pull

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Progress is one step of a pull. Stage is resolving, downloading, verifying or done.
type Progress struct {
	Stage string
	Bytes int64
	Total int64
}

// Downloader fetches artifacts with resume and verification.
type Downloader struct {
	BaseURL    string
	Token      string
	Client     *http.Client
	FreeSpace  func(dir string) (int64, error)
	OnProgress func(Progress)
}

// NewDownloader talks to huggingface.co with an optional token. Downloads have no timeout: a
// 20 GB file on a slow line takes as long as it takes, and the context cancels it.
func NewDownloader(token string) *Downloader {
	return &Downloader{BaseURL: "https://huggingface.co", Token: token, Client: &http.Client{}, FreeSpace: FreeSpace}
}

// Fetch downloads art into dest. It resumes a dest.part file, checks the free space against what is
// left, verifies the sha256 of the whole file and renames it into place. A body that ends short, a
// cancelled download and a failed one keep the part for the next attempt. A part that is larger than the
// artifact cannot be a start of it and is discarded, and the download begins again; a checksum mismatch
// of a whole file deletes it as well.
func (d *Downloader) Fetch(ctx context.Context, art Artifact, dest string) error {
	if art.Size <= 0 {
		return fmt.Errorf("pull: %s/%s reports %d bytes, so it cannot be verified", art.Repo, art.File, art.Size)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	part := dest + ".part"
	have := partSize(part)
	if have > art.Size {
		have = 0
		_ = os.Remove(part)
	}
	if err := d.checkSpace(filepath.Dir(dest), art.Size-have); err != nil {
		return err
	}
	if have < art.Size {
		if err := d.download(ctx, art, part, have); err != nil {
			return err
		}
		if got := partSize(part); got < art.Size {
			return fmt.Errorf("pull: download %s: ended short at %d of %d bytes, "+
				"the next pull resumes it", art.File, got, art.Size)
		}
	}
	d.report(Progress{Stage: "verifying", Bytes: art.Size, Total: art.Size})
	sum, err := SHA256File(part)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	if sum != art.SHA256 {
		// The part is whole here: a short one returned above, and a longer one cannot be written.
		_ = os.Remove(part)
		return fmt.Errorf("pull: %s/%s: checksum mismatch: got %s, want %s", art.Repo, art.File, sum, art.SHA256)
	}
	if err := os.Rename(part, dest); err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	d.report(Progress{Stage: "done", Bytes: art.Size, Total: art.Size})
	return nil
}

func (d *Downloader) download(ctx context.Context, art Artifact, part string, have int64) error {
	u := resolveURL(d.BaseURL, art.Repo, art.File)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := d.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("pull: download %s: %w", art.File, err)
	}
	defer func() { _ = resp.Body.Close() }()
	rep, ok := replyFor(resp.StatusCode, have, art.Size)
	if !ok {
		return fmt.Errorf("pull: download %s: Hugging Face answered %d", art.File, resp.StatusCode)
	}
	// A known length must match what this answer carries. This runs before the part is opened, so a bogus
	// answer cannot truncate a good part.
	if resp.ContentLength >= 0 && resp.ContentLength != rep.want {
		return fmt.Errorf("pull: download %s: the server sent %d bytes where %d were expected",
			art.File, resp.ContentLength, rep.want)
	}
	f, err := os.OpenFile(part, rep.flags, 0o644) //nolint:gosec // the path is built by Store, never from a request
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	// The body is capped at what this answer should carry, so a chunked body cannot write past the space
	// that was checked. The copy, the fsync and the close all run, and errors.Join reports every failure.
	body := io.LimitReader(resp.Body, rep.want)
	return errors.Join(d.copy(ctx, f, body, rep.from, art.Size), f.Sync(), f.Close())
}

// reply is how one answer lands in the part file: the open flags, the part size the body continues from,
// and the number of bytes the body should carry.
type reply struct {
	flags int
	from  int64
	want  int64
}

// replyFor maps a status to its reply. A 206 continues a part of have bytes with the rest of the file. A
// 200 carries the whole file, because the server ignored the range, and starts it over. Any other status
// is not an answer to a pull.
func replyFor(status int, have, size int64) (reply, bool) {
	base := os.O_CREATE | os.O_WRONLY
	switch {
	case status == http.StatusPartialContent && have > 0:
		return reply{flags: base | os.O_APPEND, from: have, want: size - have}, true
	case status == http.StatusOK:
		return reply{flags: base | os.O_TRUNC, from: 0, want: size}, true
	default:
		return reply{}, false
	}
}

// httpClient is the configured client, or one without a timeout for a Downloader built as a literal.
func (d *Downloader) httpClient() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return &http.Client{}
}

// resolveURL is where Hugging Face serves file of repo on the main branch. Each segment of the file
// path is escaped on its own, so a space, "?", "#" or "%" reaches the server as that name and the
// slashes between the segments stay.
func resolveURL(base, repo, file string) string {
	segments := strings.Split(file, "/")
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		escaped = append(escaped, url.PathEscape(segment))
	}
	return fmt.Sprintf("%s/%s/resolve/main/%s", base, repo, strings.Join(escaped, "/"))
}

func (d *Downloader) copy(ctx context.Context, w io.Writer, r io.Reader, done, total int64) error {
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("pull: %w", err)
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return fmt.Errorf("pull: write: %w", werr)
			}
			done += int64(n)
			d.report(Progress{Stage: "downloading", Bytes: done, Total: total})
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("pull: read: %w", err)
		}
	}
}

func (d *Downloader) checkSpace(dir string, need int64) error {
	if d.FreeSpace == nil || need <= 0 {
		return nil
	}
	free, err := d.FreeSpace(dir)
	if err != nil {
		return nil // an unknown filesystem does not block a pull
	}
	if free < need {
		return fmt.Errorf("pull: %s has %d MiB free and the download needs %d MiB more", dir, free>>20, need>>20)
	}
	return nil
}

func (d *Downloader) report(p Progress) {
	if d.OnProgress != nil {
		d.OnProgress(p)
	}
}

func partSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
