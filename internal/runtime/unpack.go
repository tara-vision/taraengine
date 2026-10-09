package runtime

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tara-vision/taraengine/internal/pull"
)

// Unpack extracts a release tarball into dest, dropping the single top-level directory the upstream
// archives carry (llama-<build>/ or cudart-llama-<build>-.../). Regular files and directories are
// written; a symlink is kept only when its target is a bare sibling file name (libllama.so ->
// libllama.so.0, as upstream ships). Any entry that would land outside dest, any absolute name, any
// other link and any other type is refused, and the error names the entry.
//
// Nothing is skipped silently. An entry that is not inside the top-level directory is an error, and so
// is an archive that holds no regular file, because an unpack that installs nothing must not read as one
// that worked. Unpack never replaces a path: an entry whose name is taken is refused, unless the path
// already holds exactly what the entry carries (the LICENSE that a second archive repeats, say). When an
// entry is refused or a write fails, Unpack removes what it created, so dest keeps only what it held
// before the call.
func Unpack(r io.Reader, dest string) (err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("runtime: unpack: %w", err)
	}
	defer func() { _ = gz.Close() }()
	var j journal
	defer func() {
		if err != nil {
			j.rollback()
		}
	}()
	tr := tar.NewReader(gz)
	for {
		hdr, nerr := tr.Next()
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			return fmt.Errorf("runtime: unpack: %w", nerr)
		}
		if err = j.entry(tr, hdr, dest); err != nil {
			return err
		}
	}
	if j.files == 0 {
		return errors.New("runtime: unpack: the archive holds no regular file under its top-level directory")
	}
	return nil
}

// journal holds the undo steps of one Unpack call, which run last in, first out when the call fails, and
// the number of regular files the archive provided.
type journal struct {
	undo  []func()
	files int
}

func (j *journal) rollback() {
	for k := len(j.undo) - 1; k >= 0; k-- {
		j.undo[k]()
	}
}

// created records that this call made path, so a rollback removes it.
func (j *journal) created(path string) {
	j.undo = append(j.undo, func() { _ = os.Remove(path) })
}

func (j *journal) entry(tr *tar.Reader, hdr *tar.Header, dest string) error {
	if hdr.Typeflag == tar.TypeXGlobalHeader {
		return nil // PAX metadata for the whole archive, not an entry
	}
	if strings.HasPrefix(hdr.Name, "/") {
		return fmt.Errorf("runtime: unpack: refusing absolute entry %q", hdr.Name)
	}
	rel, isTop, err := stripTop(hdr)
	if err != nil || isTop {
		return err
	}
	target, err := safeJoin(dest, rel)
	if err != nil {
		return err
	}
	return j.writeEntry(tr, hdr, target)
}

func (j *journal) writeEntry(tr *tar.Reader, hdr *tar.Header, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return j.mkdirAll(target)
	case tar.TypeReg:
		return j.writeFile(target, tr, hdr)
	case tar.TypeSymlink:
		return j.symlink(hdr, target)
	}
	return fmt.Errorf("runtime: unpack: refusing entry %q of type %d", hdr.Name, hdr.Typeflag)
}

func (j *journal) symlink(hdr *tar.Header, target string) error {
	if hdr.Linkname == "" || strings.ContainsAny(hdr.Linkname, "/\\") || hdr.Linkname == "." || hdr.Linkname == ".." {
		return fmt.Errorf("runtime: unpack: refusing link %q -> %q", hdr.Name, hdr.Linkname)
	}
	if err := j.mkdirAll(filepath.Dir(target)); err != nil {
		return err
	}
	err := os.Symlink(hdr.Linkname, target)
	if errors.Is(err, os.ErrExist) {
		if current, rerr := os.Readlink(target); rerr == nil && current == hdr.Linkname {
			return nil // the link is there already, pointing where the entry points
		}
		return refuseReplacing(target)
	}
	if err != nil {
		return fmt.Errorf("runtime: unpack: %w", err)
	}
	j.created(target)
	return nil
}

// writeFile creates the file for a regular entry. The create is exclusive, so it can neither replace a path
// nor follow a link that sits at the name.
func (j *journal) writeFile(target string, r io.Reader, hdr *tar.Header) error {
	if err := j.mkdirAll(filepath.Dir(target)); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, hdr.FileInfo().Mode().Perm()) //nolint:gosec,lll // target passed safeJoin
	if errors.Is(err, os.ErrExist) {
		return j.acceptOverlap(target, r, hdr.Size)
	}
	if err != nil {
		return fmt.Errorf("runtime: unpack: %w", err)
	}
	j.created(target)
	_, cerr := io.Copy(f, r) //nolint:gosec // the archive is checksum-verified before unpacking
	if err := errors.Join(cerr, f.Close()); err != nil {
		return fmt.Errorf("runtime: unpack: %w", err)
	}
	j.files++
	return nil
}

// acceptOverlap decides about an entry whose name is taken. It passes when the path is a regular file that
// holds exactly the bytes of the entry, and then counts the entry as provided, and it refuses anything else.
func (j *journal) acceptOverlap(target string, body io.Reader, size int64) error {
	same, err := holdsExactly(target, body, size)
	if err != nil {
		return fmt.Errorf("runtime: unpack: %w", err)
	}
	if !same {
		return refuseReplacing(target)
	}
	j.files++
	return nil
}

func refuseReplacing(target string) error {
	return fmt.Errorf("runtime: unpack: refusing to replace %q, something else is already there", target)
}

// holdsExactly reports whether path is a regular file of size bytes that match the next size bytes of body.
// A path that cannot be examined, because it vanished since the create failed, does not hold them.
func holdsExactly(path string, body io.Reader, size int64) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return false, nil
	}
	have, err := pull.SHA256File(path)
	if err != nil {
		return false, err
	}
	want, err := pull.SHA256Reader(io.LimitReader(body, size))
	if err != nil {
		return false, err
	}
	return have == want, nil
}

// mkdirAll creates dir and any missing parents with mode 0o750, recording each directory it made.
func (j *journal) mkdirAll(dir string) error {
	var missing []string
	for p := dir; !pathExists(p); p = filepath.Dir(p) {
		missing = append(missing, p)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("runtime: unpack: %w", err)
	}
	for k := len(missing) - 1; k >= 0; k-- {
		j.created(missing[k])
	}
	return nil
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// stripTop drops the first path element. It returns the rest of the name, and whether the entry is the
// top-level directory itself (or the "./" root before it), which has nothing to extract. An entry with no
// directory part that is not a directory cannot be placed under a top-level directory: it is an error,
// because the alternative is to drop it without a word.
func stripTop(hdr *tar.Header) (rel string, isTop bool, err error) {
	name := strings.TrimPrefix(hdr.Name, "./")
	i := strings.IndexByte(name, '/')
	if i < 0 {
		if hdr.Typeflag == tar.TypeDir {
			return "", true, nil
		}
		return "", false, fmt.Errorf("runtime: unpack: refusing %q: it is not inside the top-level directory", hdr.Name)
	}
	rel = strings.TrimLeft(name[i+1:], "/")
	return rel, rel == "", nil
}

func safeJoin(dest, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("runtime: unpack: refusing entry %q", rel)
	}
	target := filepath.Join(dest, clean)
	if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
		return "", fmt.Errorf("runtime: unpack: refusing entry %q", rel)
	}
	return target, nil
}
