// Command pinregistry fills the size and sha256 of registry entries from the Hugging Face paths-info API. An entry
// that already has both is left as it is, and then the command makes no request at all.
//
// Usage: go run ./tools/pinregistry internal/registry/registry.yaml
//
// A value counts as missing when it is empty, a placeholder such as <pin>, or, for the size, 0. The token for gated
// repositories comes from TARAENGINE_HF_TOKEN. Review the diff afterwards: a pin is a reviewed change.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/tara-vision/taraengine/internal/pull"
)

// Exit statuses of the command.
const (
	statusFailed = 1
	statusUsage  = 2
)

// line matches the keys of a registry entry that the tool reads: the name that starts an entry, the repository, and
// the file, sha256 and size of the main artifact and of each sibling. The first group is the indentation, with the
// dash of a list item when the key opens one.
var line = regexp.MustCompile(`^(\s*(?:-\s+)?)(name|hf_repo|file|sha256|size):\s*(.*)$`)

// exit ends the process. It is a variable only so that a test can run main.
var exit = os.Exit

func main() {
	exit(run(os.Args[1:], os.Getenv, os.Stderr, pull.NewResolver))
}

// run pins the registry file named in args and returns the exit status: 0 when the file is pinned, or had nothing to
// pin, 1 when it could not be read, resolved or written, and 2 for a command line of the wrong shape.
func run(
	args []string, getenv func(string) string, stderr io.Writer, newResolver func(token string) *pull.Resolver,
) int {
	if len(args) != 1 {
		report(stderr, "usage: pinregistry <registry.yaml>")
		return statusUsage
	}
	data, err := os.ReadFile(args[0]) //nolint:gosec // the path is the maintainer's own registry file, not request input
	if err != nil {
		report(stderr, err)
		return statusFailed
	}
	out, err := fill(context.Background(), newResolver(getenv("TARAENGINE_HF_TOKEN")), string(data))
	if err != nil {
		report(stderr, err)
		return statusFailed
	}
	if out == string(data) {
		return 0
	}
	if err := os.WriteFile(args[0], []byte(out), 0o644); err != nil { //nolint:gosec // the registry is public
		report(stderr, err)
		return statusFailed
	}
	return 0
}

// report prints one line to w. A write error has nowhere left to go.
func report(w io.Writer, message any) {
	_, _ = fmt.Fprintln(w, message)
}

// entry is what the walk over the registry knows about the file above the current line. art is what Hugging Face
// said about that file, and stays nil until a value needs it.
type entry struct {
	repo string
	file string
	art  *pull.Artifact
}

// fill walks the registry text line by line and returns it with every missing sha256 and size replaced by the value
// Hugging Face reports for the file above it. A "- name:" line starts an entry, so a placeholder can never take the
// values of the previous entry's file. The file is asked about once, however many of its values are missing, and
// not at all when none is.
func fill(ctx context.Context, r *pull.Resolver, text string) (string, error) {
	var cur entry
	in := strings.Split(text, "\n")
	out := make([]string, 0, len(in))
	for n, l := range in {
		m := line.FindStringSubmatch(l)
		if m == nil {
			out = append(out, l)
			continue
		}
		key, value := m[2], strings.TrimSpace(m[3])
		switch {
		case key == "name":
			cur = entry{}
		case key == "hf_repo":
			cur = entry{repo: value}
		case key == "file":
			cur = entry{repo: cur.repo, file: value}
		case missing(key, value):
			pinned, next, err := cur.pin(ctx, r, key)
			if err != nil {
				return "", fmt.Errorf("pinregistry: line %d: %w", n+1, err)
			}
			cur, l = next, m[1]+key+": "+pinned
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), nil
}

// missing reports whether the value of a sha256 or size key still has to be pinned: it is empty, a placeholder such
// as <pin>, or, for the size, 0. It is asked about those two keys only.
func missing(key, value string) bool {
	return value == "" || strings.HasPrefix(value, "<") || (key == "size" && value == "0")
}

// pin returns the value of key for the entry's file, and the entry with Hugging Face's answer kept for the next key.
func (e entry) pin(ctx context.Context, r *pull.Resolver, key string) (string, entry, error) {
	if e.art == nil {
		if e.repo == "" || e.file == "" {
			return "", e, errors.New("a value to pin comes before the hf_repo and file of its entry")
		}
		art, err := r.Resolve(ctx, e.repo, e.file)
		if err != nil {
			return "", e, err
		}
		e.art = &art
	}
	if key == "size" {
		return strconv.FormatInt(e.art.Size, 10), e, nil
	}
	return e.art.SHA256, e, nil
}
