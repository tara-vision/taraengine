// Command rendercask writes the Homebrew cask of a taraengine release, Casks/taraengine.rb, from the release's
// checksums.txt. The release workflow runs it for every stable tag and pushes the result to the tap,
// tara-vision/homebrew-taracode.
//
// Usage: go run ./tools/rendercask [-prerelease] <checksums.txt> <version>
//
// -prerelease renders a cask for a release candidate, for a local tap in a hand check; the release workflow never
// passes it, so brew upgrade never lands on a candidate.
//
// The cask is written to standard output. goreleaser writes taracode's cask, but the version the repository pins can
// only write the Ruby flight blocks that Homebrew 7 deprecates; this one uses the structured steps the cask sandbox
// spike proved (docs: the cask section of the v0.3.0 spec).
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/template"
)

// Exit statuses of the command.
const (
	statusFailed = 1
	statusUsage  = 2
)

// asset is the binary the cask installs.
const asset = "taraengine-darwin-arm64"

//go:embed cask.rb.tmpl
var caskTemplate string

// The template's own delimiters: the cask holds Homebrew's {{HOMEBREW_PREFIX}} tokens, which must pass through as they
// are.
var cask = template.Must(template.New("cask").Delims("[[", "]]").Parse(caskTemplate))

// stable is a release without a pre-release part, and candidate one with it.
var (
	stable    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	candidate = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.]+$`)
)

// exit ends the process. It is a variable only so that a test can run main.
var exit = os.Exit

func main() {
	exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run renders the cask and returns the exit status: 0 when the cask was written, 1 when the version or the checksums
// would not do, and 2 for a command line of the wrong shape.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("rendercask", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	prerelease := flags.Bool("prerelease", false, "render a cask for a release candidate, for a local tap")
	if err := flags.Parse(args); err != nil || flags.NArg() != 2 {
		_, _ = fmt.Fprintln(stderr, "usage: rendercask [-prerelease] <checksums.txt> <version>")
		return statusUsage
	}
	args = flags.Args()
	version := strings.TrimPrefix(args[1], "v")
	if !stable.MatchString(version) && (!*prerelease || !candidate.MatchString(version)) {
		_, _ = fmt.Fprintf(stderr, "rendercask: %q is not a stable release; pre-releases get no cask "+
			"(-prerelease renders one for a local tap)\n", args[1])
		return statusFailed
	}
	data, err := os.ReadFile(args[0]) //nolint:gosec // the path is the release workflow's own download
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "rendercask: %v\n", err)
		return statusFailed
	}
	sum := ""
	for _, line := range strings.Split(string(data), "\n") {
		if hash, name, ok := strings.Cut(line, "  "); ok && name == asset && len(hash) == 64 {
			sum = hash
		}
	}
	if sum == "" {
		_, _ = fmt.Fprintf(stderr, "rendercask: no sha256 for %s in %s\n", asset, args[0])
		return statusFailed
	}
	if err := cask.Execute(stdout, map[string]string{"Version": version, "SHA256": sum}); err != nil {
		_, _ = fmt.Fprintf(stderr, "rendercask: %v\n", err)
		return statusFailed
	}
	return 0
}
