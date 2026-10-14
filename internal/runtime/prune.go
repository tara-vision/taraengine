package runtime

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// buildEntry matches what an install leaves in the runtime directory: a build directory such as b11514, and the
// b11514.partial and b11514.old directories an interrupted install or swap can leave beside it.
var buildEntry = regexp.MustCompile(`^b[0-9]+(\.partial|\.old)?$`)

// RemoveOtherBuilds removes from dir every build but keep, with what interrupted installs left of them, and returns
// the names it removed. Entries that are not builds are left alone, and so are keep's own partial and old
// directories, which the installer manages. A directory that does not exist holds nothing to remove. The caller must
// hold the data directory's lock, so that no daemon runs from a build while it goes.
func RemoveOtherBuilds(dir, keep string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runtime: %w", err)
	}
	var removed []string
	var errs []error
	for _, e := range entries {
		name := e.Name()
		build, _, _ := strings.Cut(name, ".")
		if !buildEntry.MatchString(name) || build == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			errs = append(errs, fmt.Errorf("runtime: %w", err))
			continue
		}
		removed = append(removed, name)
	}
	return removed, errors.Join(errs...)
}
