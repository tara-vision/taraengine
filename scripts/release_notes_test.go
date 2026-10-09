package scripts

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const sampleChangelog = `# Changelog

## [Unreleased]

### Added
- an unreleased thing

## [0.2.0] - 2026-11-01

### Fixed
- the 0.2.0 fix

- a second paragraph

## [0.4.0] - 2026-12-01

## [0.1.0] - 2026-10-10

### Added
- the first release

[0.2.0]: https://example.invalid/compare/v0.1.0...v0.2.0
[0.1.0]: https://example.invalid/releases/tag/v0.1.0
`

// releaseNotes runs scripts/release-notes.sh with the given arguments and returns its exit status, standard output and
// standard error.
func releaseNotes(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"release-notes.sh"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exited *exec.ExitError
	switch {
	case err == nil:
		return 0, stdout.String(), stderr.String()
	case errors.As(err, &exited):
		return exited.ExitCode(), stdout.String(), stderr.String()
	}
	t.Fatalf("run release-notes.sh: %v", err)
	return 0, "", ""
}

func writeChangelog(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "CHANGELOG.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReleaseNotesPrintTheSectionOfTheTag(t *testing.T) {
	changelog := writeChangelog(t, sampleChangelog)
	cases := []struct{ name, tag, want string }{
		{"a middle section", "v0.2.0", "### Fixed\n- the 0.2.0 fix\n\n- a second paragraph\n"},
		{"the last section, before the link references", "v0.1.0", "### Added\n- the first release\n"},
		{"a version without the v", "0.1.0", "### Added\n- the first release\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errText := releaseNotes(t, tc.tag, changelog)
			if code != 0 || out != tc.want || errText != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q, want exit 0 and %q", code, out, errText, tc.want)
			}
		})
	}
}

func TestReleaseNotesRefuseASectionThatIsNotThere(t *testing.T) {
	changelog := writeChangelog(t, sampleChangelog)
	cases := []struct{ name, tag, want string }{
		{"unknown version", "v0.3.0", "0.3.0"},
		{"a prefix of a version", "v0.1", "0.1"},
		{"a section with no text", "v0.4.0", "0.4.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errText := releaseNotes(t, tc.tag, changelog)
			if code != 1 || out != "" || !strings.Contains(errText, tc.want) {
				t.Fatalf("exit %d, stdout %q, stderr %q, want exit 1 with %q named", code, out, errText, tc.want)
			}
		})
	}
}

func TestReleaseNotesNeedATagAndAFile(t *testing.T) {
	if code, _, errText := releaseNotes(t); code != 2 || !strings.Contains(errText, "usage") {
		t.Fatalf("without a tag: exit %d, stderr %q", code, errText)
	}
	missing := filepath.Join(t.TempDir(), "nothing.md")
	if code, _, errText := releaseNotes(t, "v0.1.0", missing); code != 1 || !strings.Contains(errText, missing) {
		t.Fatalf("without the file: exit %d, stderr %q", code, errText)
	}
}

// TestReleaseNotesReadTheChangelogOfTheRepositoryByDefault takes the newest released section of the real CHANGELOG.md,
// whatever version that is, and asks for it without naming the file.
func TestReleaseNotesReadTheChangelogOfTheRepositoryByDefault(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "CHANGELOG.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindSubmatch(data)
	if m == nil {
		t.Fatal("CHANGELOG.md has no released section")
	}
	code, out, errText := releaseNotes(t, "v"+string(m[1]))
	if code != 0 || strings.TrimSpace(out) == "" || errText != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q for version %s", code, out, errText, m[1])
	}
}
