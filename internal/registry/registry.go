// Package registry is the curated model catalog: short names pinned to Hugging Face artifacts.
package registry

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed registry.yaml
var embedded []byte

// Tiers are the host RAM classes, smallest first.
var Tiers = []string{"small", "16", "32", "48"}

// Sibling is a file pulled next to the main GGUF: a multimodal projector or a speculative-decoding draft. It comes
// from the entry's repository unless it names one of its own (a drafter published apart from its model).
type Sibling struct {
	File   string `yaml:"file"`
	SHA256 string `yaml:"sha256"`
	Size   int64  `yaml:"size"`
	HFRepo string `yaml:"hf_repo"`
}

// Entry is one catalog model.
type Entry struct {
	Name         string            `yaml:"name"`
	Family       string            `yaml:"family"`
	Params       string            `yaml:"params"`
	HFRepo       string            `yaml:"hf_repo"`
	File         string            `yaml:"file"`
	SHA256       string            `yaml:"sha256"`
	Size         int64             `yaml:"size"`
	Quantization string            `yaml:"quantization"`
	Context      int               `yaml:"context"`
	Capabilities []string          `yaml:"capabilities"`
	Tier         string            `yaml:"tier"`
	Default      bool              `yaml:"default"`
	MMProj       *Sibling          `yaml:"mmproj"`
	MTP          *Sibling          `yaml:"mtp"`    // a multi-token-prediction draft, served as draft-mtp
	DFlash       *Sibling          `yaml:"dflash"` // a DFlash drafter, served as draft-dflash; never together with mtp
	Preset       map[string]string `yaml:"preset"` // tuning keys only, see tuningKeyList
	MinRuntime   string            `yaml:"min_runtime"`
	Notes        string            `yaml:"notes"`
}

// Registry is the parsed catalog.
type Registry struct {
	Entries []Entry `yaml:"entries"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hfRepoPattern is the owner/name shape of a Hugging Face repository. The repository becomes a path in
// the model store, so nothing outside this shape may pass. The pull package uses the same pattern.
var hfRepoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Load parses the embedded catalog.
func Load() (*Registry, error) { return Parse(embedded) }

// LoadFile parses a catalog file, the override a data directory may carry.
func LoadFile(path string) (*Registry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the operator's own catalog file, not request input
	if err != nil {
		return nil, fmt.Errorf("registry: read %s: %w", path, err)
	}
	return Parse(data)
}

// Parse validates every entry: a valid name, a repository, relative .gguf files with a sha256 and a
// size, a known tier, tuning keys only in the preset, no duplicate names, no two names the router would
// serve under one id, at most one default per tier. A key the catalog does not know is refused with the
// keys it accepts, so a misspelled key never passes unnoticed.
func Parse(data []byte) (*Registry, error) {
	var r Registry
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil && !errors.Is(err, io.EOF) { // EOF: the file holds no document
		return nil, decodeError(err)
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	return &r, nil
}

// check validates the entries one by one and then against each other.
func (r Registry) check() error {
	defaults := map[string]string{}
	seen := map[string]bool{}
	ids := map[string]string{} // router id -> the name that took it
	for _, e := range r.Entries {
		if err := e.validate(); err != nil {
			return err
		}
		if seen[e.Name] {
			return fmt.Errorf("registry: %s listed twice", e.Name)
		}
		seen[e.Name] = true
		id := SectionID(e.Name)
		if other, taken := ids[id]; taken {
			return fmt.Errorf("registry: %s and %s share the router id %q", other, e.Name, id)
		}
		ids[id] = e.Name
		if e.Default {
			if other, ok := defaults[e.Tier]; ok {
				return fmt.Errorf("registry: tier %s has two defaults, %s and %s", e.Tier, other, e.Name)
			}
			defaults[e.Tier] = e.Name
		}
	}
	return nil
}

// unknownField is the message yaml.v3 gives for a key that KnownFields refused.
var unknownField = regexp.MustCompile(`^line (\d+): field (.+) not found in type (\S+)$`)

// acceptedKeys are the keys of each type a catalog decodes into, under the name yaml.v3 gives the type.
var acceptedKeys = map[string]string{
	"registry.Registry": yamlKeys(reflect.TypeFor[Registry]()),
	"registry.Entry":    yamlKeys(reflect.TypeFor[Entry]()),
	"registry.Sibling":  yamlKeys(reflect.TypeFor[Sibling]()),
}

// yamlKeys lists the yaml names of a struct's fields in declaration order.
func yamlKeys(t reflect.Type) string {
	keys := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		keys = append(keys, name)
	}
	return strings.Join(keys, ", ")
}

// decodeError is the decoder's error with every unknown key named together with the keys accepted in its place.
func decodeError(err error) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return fmt.Errorf("registry: parse: %w", err)
	}
	problems := make([]string, 0, len(typeErr.Errors))
	for _, msg := range typeErr.Errors {
		if m := unknownField.FindStringSubmatch(msg); m != nil {
			msg = fmt.Sprintf("line %s: unknown key %q; accepted keys: %s", m[1], m[2], acceptedKeys[m[3]])
		}
		problems = append(problems, msg)
	}
	return fmt.Errorf("registry: parse: %s", strings.Join(problems, "; "))
}

func (e Entry) validate() error {
	if err := ValidName(e.Name); err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	if !hfRepoPattern.MatchString(e.HFRepo) || strings.Contains(e.HFRepo, "..") {
		return fmt.Errorf("registry: %s hf_repo %q must be owner/name", e.Name, e.HFRepo)
	}
	if !knownTier(e.Tier) {
		return fmt.Errorf("registry: %s has unknown tier %q", e.Name, e.Tier)
	}
	if e.MTP != nil && e.DFlash != nil {
		return fmt.Errorf("registry: %s has both mtp and dflash drafts; a model takes one", e.Name)
	}
	for _, f := range e.Files() {
		if err := f.validate(e.Name); err != nil {
			return err
		}
	}
	return checkPreset(e.Name, e.Preset)
}

func (s Sibling) validate(owner string) error {
	if !strings.HasSuffix(s.File, ".gguf") || strings.Contains(s.File, "..") || strings.HasPrefix(s.File, "/") {
		return fmt.Errorf("registry: %s file %q must be a relative .gguf path", owner, s.File)
	}
	if !sha256Hex.MatchString(s.SHA256) {
		return fmt.Errorf("registry: %s file %s has no valid sha256", owner, s.File)
	}
	if s.Size <= 0 {
		return fmt.Errorf("registry: %s file %s has no size", owner, s.File)
	}
	if s.HFRepo != "" && (!hfRepoPattern.MatchString(s.HFRepo) || strings.Contains(s.HFRepo, "..")) {
		return fmt.Errorf("registry: %s file %s hf_repo %q must be owner/name", owner, s.File, s.HFRepo)
	}
	return nil
}

func knownTier(t string) bool {
	for _, k := range Tiers {
		if k == t {
			return true
		}
	}
	return false
}

// Files lists the entry's artifacts: the main file, then the projector, then the draft of either kind.
func (e Entry) Files() []Sibling {
	files := []Sibling{{File: e.File, SHA256: e.SHA256, Size: e.Size}}
	if e.MMProj != nil {
		files = append(files, *e.MMProj)
	}
	if d, _ := e.Draft(); d != nil {
		files = append(files, *d)
	}
	return files
}

// RepoOf is the repository a file of the entry comes from: the sibling's own when it names one, else the entry's.
func (e Entry) RepoOf(s Sibling) string {
	if s.HFRepo != "" {
		return s.HFRepo
	}
	return e.HFRepo
}

// Draft returns the entry's draft model and its kind, "mtp" or "dflash", or nil and "" without one.
func (e Entry) Draft() (*Sibling, string) {
	switch {
	case e.DFlash != nil:
		return e.DFlash, "dflash"
	case e.MTP != nil:
		return e.MTP, "mtp"
	}
	return nil, ""
}

// Find returns the entry with that exact name.
func (r *Registry) Find(name string) (Entry, bool) {
	for _, e := range r.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Tier lists the entries of a tier in catalog order.
func (r *Registry) Tier(tier string) []Entry {
	var out []Entry
	for _, e := range r.Entries {
		if e.Tier == tier {
			out = append(out, e)
		}
	}
	return out
}

// Default is the tier's recommended entry.
func (r *Registry) Default(tier string) (Entry, bool) {
	for _, e := range r.Entries {
		if e.Tier == tier && e.Default {
			return e, true
		}
	}
	return Entry{}, false
}
