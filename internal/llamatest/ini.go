// Package llamatest is a scripted llama-server in router mode for tests: the same URLs, JSON shapes
// and status transitions as upstream build b11514, without a model.
package llamatest

import (
	"maps"
	"slices"
	"strings"
)

// ParseINI reads the preset format: [section] headers, key = value lines, # comments. Lines that
// are neither are ignored, as the router ignores them.
func ParseINI(data []byte) map[string]map[string]string {
	out := map[string]map[string]string{}
	section := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			if _, ok := out[section]; !ok {
				out[section] = map[string]string{}
			}
		default:
			k, v, ok := strings.Cut(line, "=")
			if ok && section != "" {
				out[section][strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

// renderSection is the preset text the router echoes per model. The router echoes long option
// names, so the short key c is printed as ctx-size; every other key is printed as written.
func renderSection(id string, keys map[string]string) string {
	var b strings.Builder
	b.WriteString("[" + id + "]\n")
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		name := k
		if k == "c" {
			name = "ctx-size"
		}
		b.WriteString(name + " = " + keys[k] + "\n")
	}
	return b.String()
}
