package pull

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/tara-vision/taraengine/internal/registry"
)

// RefKind says where a pull reference points.
type RefKind int

// The three kinds of reference `taraengine pull` accepts.
const (
	KindRegistry RefKind = iota // a catalog name such as gemma4:12b
	KindHF                      // hf://owner/repo/path/to/file.gguf
	KindLocal                   // an absolute path to a .gguf on this machine
)

// Ref is a parsed pull reference. Name is the model name: the catalog name, or the file name
// without its .gguf suffix for the escape hatches.
type Ref struct {
	Kind RefKind
	Name string
	Repo string
	File string
	Path string
}

// ParseRef classifies s as hf://owner/repo/file.gguf, an absolute .gguf path, or a catalog name.
func ParseRef(s string) (Ref, error) {
	switch {
	case strings.HasPrefix(s, "hf://"):
		return parseHF(s)
	case filepath.IsAbs(s):
		if !strings.HasSuffix(s, ".gguf") {
			return Ref{}, fmt.Errorf("pull: %q must be a .gguf file", s)
		}
		name := strings.TrimSuffix(filepath.Base(s), ".gguf")
		if err := registry.ValidName(name); err != nil {
			return Ref{}, fmt.Errorf("pull: %w", err)
		}
		return Ref{Kind: KindLocal, Name: name, Path: filepath.Clean(s)}, nil
	default:
		if err := registry.ValidName(s); err != nil {
			return Ref{}, fmt.Errorf("pull: %w", err)
		}
		return Ref{Kind: KindRegistry, Name: s}, nil
	}
}

func parseHF(s string) (Ref, error) {
	parts := strings.SplitN(strings.TrimPrefix(s, "hf://"), "/", 3)
	if len(parts) != 3 || parts[2] == "" {
		return Ref{}, fmt.Errorf("pull: %q must be hf://owner/repo/file.gguf", s)
	}
	repo := parts[0] + "/" + parts[1]
	if err := ValidRepo(repo); err != nil {
		return Ref{}, err
	}
	file := parts[2]
	if !strings.HasSuffix(file, ".gguf") || strings.Contains(file, "..") {
		return Ref{}, fmt.Errorf("pull: %q must name a .gguf file", s)
	}
	name := strings.TrimSuffix(path.Base(file), ".gguf")
	if err := registry.ValidName(name); err != nil {
		return Ref{}, fmt.Errorf("pull: %w", err)
	}
	return Ref{Kind: KindHF, Name: name, Repo: repo, File: file}, nil
}
