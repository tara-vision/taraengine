package registry

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// ValidName accepts a model name of up to 64 characters: letters, digits, dot, underscore, colon and
// hyphen, starting with a letter or a digit, never containing "..", so a name can never carry a path.
func ValidName(name string) error {
	if name == "" {
		return fmt.Errorf("model name is empty")
	}
	if !utf8.ValidString(name) || !namePattern.MatchString(name) || strings.Contains(name, "..") {
		return fmt.Errorf("model name %q: use letters, digits, dot, underscore, colon and hyphen, up to 64 characters", name)
	}
	return nil
}

// SectionID is the id the router serves a model under: the preset section name, with every colon mapped to a
// hyphen, because llama-server rewrites a colon in a section name (gemma4:12b would be served as gemma4:12B). The
// mapping is not one-to-one: a:b and a-b share the id a-b, so two such names can never be served side by side.
func SectionID(name string) string { return strings.ReplaceAll(name, ":", "-") }
