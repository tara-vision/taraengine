package pull

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store is the blob directory: <data>/models/<owner>/<repo>/<file> for Hugging Face artifacts, and
// <data>/models/local/<name>.gguf as symlinks for local files.
type Store struct {
	Dir string
}

// Path is where repo's file lives; nested file paths are kept (tinyllamas/stories260K.gguf). Path does not
// check its names. Use Locate for a path that must stay inside the store.
func (s Store) Path(repo, file string) string {
	return filepath.Join(s.Dir, filepath.FromSlash(repo), filepath.FromSlash(file))
}

// Locate is Path for a name that must stay in the store. It refuses a repo or file whose joined path would
// leave the store root, so a ".." segment cannot reach a file outside it.
func (s Store) Locate(repo, file string) (string, error) {
	p := s.Path(repo, file)
	if !within(s.Dir, p) {
		return "", fmt.Errorf("pull: %s/%s would leave the store", repo, file)
	}
	return p, nil
}

// Exists reports whether the blob is fully in place; a .part file does not count. A name that would leave
// the store is never in it.
func (s Store) Exists(repo, file string) bool {
	p, err := s.Locate(repo, file)
	if err != nil {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// Remove deletes the given files of repo and their part files, then prunes the directories they leave
// empty, up to but never including the store root. A missing file is not an error. Every name is checked
// before anything is removed, so one name that would leave the store refuses the whole call.
func (s Store) Remove(repo string, files ...string) error {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		p, err := s.Locate(repo, f)
		if err != nil {
			return err
		}
		paths = append(paths, p)
	}
	for _, p := range paths {
		for _, victim := range []string{p, p + ".part"} {
			if err := os.Remove(victim); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("pull: remove %s: %w", victim, err)
			}
		}
		s.prune(filepath.Dir(p))
	}
	return nil
}

func (s Store) prune(dir string) {
	root := filepath.Clean(s.Dir)
	for dir != root && strings.HasPrefix(dir, root+string(os.PathSeparator)) {
		if err := os.Remove(dir); err != nil {
			return // not empty, or already gone
		}
		dir = filepath.Dir(dir)
	}
}

// LinkLocal exposes an absolute local .gguf as models/local/<name>.gguf and returns the link path. A symlink
// that an earlier pull left there is replaced. A regular file or a directory there is an error and stays.
func (s Store) LinkLocal(name, absPath string) (string, error) {
	if !filepath.IsAbs(absPath) {
		return "", fmt.Errorf("pull: %s is not an absolute path", absPath)
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("pull: %s is not a readable file", absPath)
	}
	dir := filepath.Join(s.Dir, "local")
	link := filepath.Join(dir, name+".gguf")
	if !within(dir, link) {
		return "", fmt.Errorf("pull: %s would leave the local links", name)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("pull: %w", err)
	}
	if err := relink(absPath, link); err != nil {
		return "", fmt.Errorf("pull: link %s: %w", link, err)
	}
	return link, nil
}

// relink points link at absPath. It replaces a symlink at link and refuses anything else there. A symlink
// that cannot be removed is reported by the symlink call that follows.
func relink(absPath, link string) error {
	info, err := os.Lstat(link)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return errors.New("not a symlink, so it was left alone")
		}
		_ = os.Remove(link)
	}
	return os.Symlink(absPath, link)
}

// within reports whether p lies strictly inside root.
func within(root, p string) bool {
	return strings.HasPrefix(filepath.Clean(p), filepath.Clean(root)+string(os.PathSeparator))
}
