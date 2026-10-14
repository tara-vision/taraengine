// Package selfupdate finds the latest taraengine release and puts it in place: it looks the release up, orders
// versions as SemVer does, tells how the running binary was installed, and downloads and checks what it installs.
package selfupdate

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// versionPattern is a release version: MAJOR.MINOR.PATCH with an optional pre-release such as rc.1, and an optional
// leading v. Build metadata does not occur in our tags and is refused.
var versionPattern = regexp.MustCompile(`^v?` + number + `\.` + number + `\.` + number +
	`(?:-([0-9A-Za-z]+(?:\.[0-9A-Za-z]+)*))?$`)

// number is one part of the version core: no leading zero.
const number = `(0|[1-9][0-9]*)`

// Version is a parsed release version.
type Version struct {
	core [3]int
	pre  []string
	text string
}

// ParseVersion reads a release version, with or without its leading v.
func ParseVersion(s string) (Version, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("selfupdate: %q is not a release version", s)
	}
	v := Version{text: strings.TrimPrefix(s, "v")}
	for i := range 3 {
		v.core[i], _ = strconv.Atoi(m[i+1]) // the pattern admits digits only, of a sane length
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, nil
}

// String is the version without its leading v.
func (v Version) String() string { return v.text }

// Prerelease reports whether the version is a release candidate or another pre-release.
func (v Version) Prerelease() bool { return len(v.pre) > 0 }

// Compare is -1, 0 or 1 as v is older than, the same as, or newer than o, in SemVer's order: a pre-release comes before
// its release, numeric identifiers compare as numbers and before the others, and a longer pre-release wins when the
// shared part is equal.
func (v Version) Compare(o Version) int {
	if c := slices.Compare(v.core[:], o.core[:]); c != 0 {
		return c
	}
	switch {
	case len(v.pre) == 0 && len(o.pre) == 0:
		return 0
	case len(v.pre) == 0:
		return 1
	case len(o.pre) == 0:
		return -1
	}
	for i := range min(len(v.pre), len(o.pre)) {
		if c := compareIdentifier(v.pre[i], o.pre[i]); c != 0 {
			return c
		}
	}
	return cmp.Compare(len(v.pre), len(o.pre))
}

func compareIdentifier(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		return cmp.Compare(na, nb)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}
