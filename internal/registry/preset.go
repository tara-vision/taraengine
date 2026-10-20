package registry

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// tuningKeyList is every key a registry preset may carry: llama-server options that tune how a model runs. The
// engine renders the keys that place a model itself (model, c or ctx-size, mmproj, model-draft, spec-type and their
// short forms), and the router owns the keys that shape the server and its network face (host, port, api-key,
// api-key-file, alias, hf-repo, hf-file, model-url, log-file, sleep-idle-seconds and every models-* key), so none
// of those may come from the catalog.
var tuningKeyList = []string{
	"n-gpu-layers", "ngl", "flash-attn", "fa",
	"cache-type-k", "cache-type-v", "ctk", "ctv",
	"temp", "top-k", "top-p", "min-p", "repeat-penalty",
	"batch-size", "ubatch-size", "threads", "n-predict",
	"jinja", "reasoning-format", "reasoning-budget", "chat-template-kwargs",
	"spec-draft-n-max", "spec-draft-n-min", "spec-draft-p-min", "draft-p-min",
	"spec-draft-ngl", "ngld", "gpu-layers-draft", "n-gpu-layers-draft",
	"mlock", "no-mmap",
}

// renamedKeys are the keys llama-server removed in favor of a new spelling (b11750: "the argument has been removed"),
// so a catalog that still carries one is told the new name instead of the whole list.
var renamedKeys = map[string]string{"draft-max": "spec-draft-n-max", "draft-min": "spec-draft-n-min"}

var tuningKeys = func() map[string]bool {
	out := make(map[string]bool, len(tuningKeyList))
	for _, k := range tuningKeyList {
		out[k] = true
	}
	return out
}()

// checkPreset refuses a preset key that is not a tuning key. Keys are checked in sorted order, so the same
// entry always reports the same key first.
func checkPreset(owner string, preset map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(preset)) {
		if renamed, ok := renamedKeys[key]; ok {
			return fmt.Errorf("registry: %s preset key %q is not a tuning key; llama-server renamed it to %s",
				owner, key, renamed)
		}
		if !tuningKeys[key] {
			return fmt.Errorf("registry: %s preset key %q is not a tuning key; accepted keys: %s",
				owner, key, strings.Join(tuningKeyList, ", "))
		}
	}
	return nil
}

// presetAliases are the short names of the tuning keys that have one.
var presetAliases = map[string]string{
	"ngl": "n-gpu-layers", "fa": "flash-attn", "ctk": "cache-type-k", "ctv": "cache-type-v",
	"draft-p-min": "spec-draft-p-min",
	"ngld":        "spec-draft-ngl", "gpu-layers-draft": "spec-draft-ngl", "n-gpu-layers-draft": "spec-draft-ngl",
}

// CanonicalKey returns a tuning key's long name, so a key and its short name compare equal.
func CanonicalKey(key string) string {
	if long, ok := presetAliases[key]; ok {
		return long
	}
	return key
}
