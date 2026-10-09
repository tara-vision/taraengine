package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedCatalogLoads(t *testing.T) {
	r, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(r.Entries) != 8 {
		t.Fatalf("entries %d, want 8", len(r.Entries))
	}
	for _, tier := range Tiers {
		if _, ok := r.Default(tier); !ok {
			t.Errorf("tier %s has no default", tier)
		}
	}
	e, ok := r.Find("gemma4:12b")
	if !ok || e.HFRepo != "ggml-org/gemma-4-12B-it-GGUF" || e.MMProj == nil || e.MTP == nil || !e.Default || e.Tier != "16" {
		t.Fatalf("gemma4:12b entry wrong: %+v", e)
	}
	if got := len(e.Files()); got != 3 {
		t.Fatalf("gemma4:12b files %d, want 3", got)
	}
	glm, _ := r.Find("glm-4.7-flash")
	if len(glm.Files()) != 1 || glm.Files()[0].File != "GLM-4.7-Flash-Q4_K.gguf" {
		t.Fatalf("glm files: %+v", glm.Files())
	}
	if got := len(r.Tier("32")); got != 4 {
		t.Fatalf("tier 32 has %d entries, want 4", got)
	}
	if _, ok := r.Find("nope"); ok {
		t.Fatal("found a model that does not exist")
	}
}

func TestParseRejectsBrokenCatalogs(t *testing.T) {
	valid := `
entries:
  - name: a:1
    family: a
    hf_repo: org/repo
    file: a.gguf
    sha256: ` + strings.Repeat("a", 64) + `
    size: 10
    tier: "16"
    default: true
`
	if _, err := Parse([]byte(valid)); err != nil {
		t.Fatalf("valid catalog refused: %v", err)
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"not yaml", "entries: [", "parse"},
		{"bad name", strings.Replace(valid, "name: a:1", "name: ../a", 1), "model name"},
		{"no repo", strings.Replace(valid, "hf_repo: org/repo", "hf_repo: \"\"", 1), "owner/name"},
		{"repo escapes the store", strings.Replace(valid, "hf_repo: org/repo", "hf_repo: \"../../../../tmp/x\"", 1), "owner/name"},
		{"repo with three parts", strings.Replace(valid, "hf_repo: org/repo", "hf_repo: a/b/c", 1), "owner/name"},
		{"repo with dot dot inside", strings.Replace(valid, "hf_repo: org/repo", "hf_repo: a/b..c", 1), "owner/name"},
		{"absolute file", strings.Replace(valid, "file: a.gguf", "file: /etc/a.gguf", 1), "relative .gguf"},
		{"not gguf", strings.Replace(valid, "file: a.gguf", "file: a.bin", 1), "relative .gguf"},
		{"dot dot file", strings.Replace(valid, "file: a.gguf", "file: ../a.gguf", 1), "relative .gguf"},
		{"bad sha", strings.Replace(valid, strings.Repeat("a", 64), "zz", 1), "sha256"},
		{"no size", strings.Replace(valid, "size: 10", "size: 0", 1), "no size"},
		{"unknown tier", strings.Replace(valid, "tier: \"16\"", "tier: \"64\"", 1), "unknown tier"},
		{"duplicate", valid + strings.TrimPrefix(valid, "\nentries:"), "listed twice"},
		{"two defaults", valid + strings.Replace(strings.TrimPrefix(valid, "\nentries:"), "name: a:1", "name: b:1", 1), "two defaults"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.yaml")
	if _, err := LoadFile(path); err == nil {
		t.Fatal("missing file must fail")
	}
	data, _ := os.ReadFile("registry.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadFile(path)
	if err != nil || len(r.Entries) != 8 {
		t.Fatalf("load file: %v, %d entries", err, len(r.Entries))
	}
}

func TestDefaultIsAbsentForAnUnknownTier(t *testing.T) {
	r, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := r.Default("64"); ok {
		t.Fatal("an unknown tier must have no default")
	}
}
