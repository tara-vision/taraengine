// Package llamaserver runs llama-server in router mode behind the backend interface.
package llamaserver

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/tara-vision/taraengine/internal/registry"
)

// PresetModel is one section of the preset file.
type PresetModel struct {
	Name       string
	ModelPath  string
	MMProjPath string
	DraftPath  string
	DraftKind  string // "mtp" (the default with a draft path) or "dflash"
	Context    int
	Extra      map[string]string // tuning keys only; engine-owned keys win over them
}

// DefaultGlobal is the [*] section: jinja templates with native tool calls, flash attention when the
// backend supports it, every layer on the GPU, thoughts in reasoning_content, nothing loaded at start, and
// one slot: the engine serves one request at a time, so a loaded model's memory is the single-user figure the
// boards publish; preset_global {parallel: "4"} brings llama-server's automatic slot count back.
func DefaultGlobal() map[string]string {
	return map[string]string{
		"jinja":            "true",
		"flash-attn":       "auto",
		"n-gpu-layers":     "999",
		"parallel":         "1",
		"reasoning-format": "deepseek",
		"load-on-startup":  "false",
	}
}

// SectionID is the router model id for a catalog name, registry.SectionID: every colon becomes a hyphen,
// because the real llama-server rewrites a colon in a preset section name. The mapping is not one-to-one,
// so the backend serves only the first of two names that share an id.
func SectionID(name string) string { return registry.SectionID(name) }

// RenderPresets renders the [*] section, then one section per model in the given order, keys
// sorted, so the same input always produces the same file.
func RenderPresets(global map[string]string, models []PresetModel) string {
	var b strings.Builder
	b.WriteString("[*]\n")
	writeKeys(&b, global)
	for _, m := range models {
		b.WriteString("\n[" + SectionID(m.Name) + "]\n")
		writeKeys(&b, m.keys())
	}
	return b.String()
}

// keys merges the extras with the generated keys, and a generated key replaces an extra of the same
// name. A registry c or ctx-size applies only when the model has no context of its own.
func (m PresetModel) keys() map[string]string {
	keys := make(map[string]string, len(m.Extra))
	for k, v := range m.Extra {
		keys[k] = v
	}
	if m.Context > 0 {
		delete(keys, "c")
		delete(keys, "ctx-size")
		keys["c"] = strconv.Itoa(m.Context)
	}
	keys["model"] = m.ModelPath
	if m.MMProjPath != "" {
		keys["mmproj"] = m.MMProjPath
	}
	if m.DraftPath != "" {
		keys["model-draft"] = m.DraftPath
		keys["spec-type"] = "draft-mtp"
		if m.DraftKind == "dflash" {
			keys["spec-type"] = "draft-dflash"
		}
	}
	return keys
}

func writeKeys(b *strings.Builder, keys map[string]string) {
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Fprintf(b, "%s = %s\n", k, keys[k])
	}
}

// WritePresets writes the file through a temporary name and a rename, so the router never reads
// a half-written file. A failed write removes the temporary file.
func WritePresets(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil { //nolint:gosec // the preset file is not secret
		_ = os.Remove(tmp)
		return fmt.Errorf("llamaserver: write presets: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("llamaserver: write presets: %w", err)
	}
	return nil
}
