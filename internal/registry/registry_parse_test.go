package registry

import (
	"strings"
	"testing"
)

// entryYAML is one valid entry under the given name, indented as an item of entries, with extra lines appended.
func entryYAML(name, file string, extra ...string) string {
	lines := []string{
		"  - name: " + name,
		"    family: a",
		"    hf_repo: org/repo",
		"    file: " + file,
		"    sha256: " + strings.Repeat("a", 64),
		"    size: 10",
		`    tier: "16"`,
	}
	for _, e := range extra {
		lines = append(lines, "    "+e)
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestParseNamesAnUnknownKeyAndTheAcceptedOnes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			"an entry key spelled with a hyphen",
			"entries:\n" + entryYAML("a:1", "a.gguf", "min-runtime: b11514"),
			[]string{`unknown key "min-runtime"`, "line 9", "accepted keys: name, family, params, hf_repo, file, sha256, size"},
		},
		{
			"a misspelled top-level key",
			"entires:\n" + entryYAML("a:1", "a.gguf"),
			[]string{`unknown key "entires"`, "line 1", "accepted keys: entries"},
		},
		{
			"a misspelled sibling key",
			"entries:\n" + entryYAML("a:1", "a.gguf", "mmproj:", "  file: p.gguf", "  sha: x"),
			[]string{`unknown key "sha"`, "line 11", "accepted keys: file, sha256, size, hf_repo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.body))
			if err == nil {
				t.Fatal("an unknown key was accepted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestParseKeepsOtherDecodeErrors(t *testing.T) {
	_, err := Parse([]byte("entries:\n" + strings.Replace(entryYAML("a:1", "a.gguf"), "size: 10", "size: ten", 1)))
	if err == nil || !strings.Contains(err.Error(), "registry: parse") || !strings.Contains(err.Error(), "ten") {
		t.Fatalf("a value of the wrong type must be reported as yaml says it: %v", err)
	}
}

func TestParseReadsAnEmptyCatalog(t *testing.T) {
	for _, body := range []string{"", "# only a comment\n"} {
		r, err := Parse([]byte(body))
		if err != nil || len(r.Entries) != 0 {
			t.Fatalf("%q: an empty catalog has no entries and no error: %+v %v", body, r, err)
		}
	}
}

func TestParseRefusesNamesThatShareARouterID(t *testing.T) {
	body := "entries:\n" + entryYAML("x:y", "a.gguf") + entryYAML("x-y", "b.gguf")
	_, err := Parse([]byte(body))
	if err == nil || !strings.Contains(err.Error(), `x:y and x-y share the router id "x-y"`) {
		t.Fatalf("two names the router serves under one id must be refused: %v", err)
	}
}

func TestParseAcceptsOnlyTuningKeysInAPreset(t *testing.T) {
	for _, key := range tuningKeyList {
		body := "entries:\n" + entryYAML("a:1", "a.gguf", "preset:", "  "+key+": x")
		if _, err := Parse([]byte(body)); err != nil {
			t.Errorf("tuning key %q refused: %v", key, err)
		}
	}
	refused := []string{
		"model", "m", "c", "ctx-size", "mmproj", "model-draft", "md", "spec-type",
		"host", "port", "api-key", "api-key-file", "alias", "hf-repo", "hf-file", "model-url",
		"models-dir", "models-max", "models-preset", "models-autoload", "models-anything", "sleep-idle-seconds",
		"log-file", "load-on-startup",
	}
	for _, key := range refused {
		body := "entries:\n" + entryYAML("a:1", "a.gguf", "preset:", "  "+key+": x")
		_, err := Parse([]byte(body))
		if err == nil || !strings.Contains(err.Error(), `preset key "`+key+`" is not a tuning key`) ||
			!strings.Contains(err.Error(), "accepted keys: n-gpu-layers") {
			t.Errorf("key %q must be refused with the accepted keys: %v", key, err)
		}
	}
}

func TestEmbeddedCatalogUsesOnlyTuningKeys(t *testing.T) {
	r, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range r.Entries {
		for key := range e.Preset {
			if !tuningKeys[key] {
				t.Errorf("%s: preset key %q is not a tuning key", e.Name, key)
			}
		}
	}
}

func TestParseAcceptsADFlashDrafterInItsOwnRepository(t *testing.T) {
	body := "entries:\n" + entryYAML("a:1", "a.gguf",
		"mmproj:", "  file: p.gguf", "  sha256: "+strings.Repeat("b", 64), "  size: 2",
		"dflash:", "  hf_repo: org/drafter", "  file: d.gguf", "  sha256: "+strings.Repeat("c", 64), "  size: 3")
	r, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	e := r.Entries[0]
	if e.DFlash == nil || e.DFlash.HFRepo != "org/drafter" || e.DFlash.File != "d.gguf" {
		t.Fatalf("dflash %+v", e.DFlash)
	}
	if got := e.RepoOf(*e.DFlash); got != "org/drafter" {
		t.Errorf("RepoOf(dflash) = %q, want the sibling's own repository", got)
	}
	if got := e.RepoOf(*e.MMProj); got != "org/repo" {
		t.Errorf("RepoOf(mmproj) = %q, want the entry's repository", got)
	}
	if d, kind := e.Draft(); d == nil || kind != "dflash" || d.File != "d.gguf" {
		t.Errorf("Draft() = %v, %q", d, kind)
	}
	if files := e.Files(); len(files) != 3 || files[2].File != "d.gguf" || files[2].HFRepo != "org/drafter" {
		t.Errorf("Files() = %+v, want the main file, the projector and the drafter", files)
	}
}

func TestParseRefusesBothDraftKinds(t *testing.T) {
	body := "entries:\n" + entryYAML("a:1", "a.gguf",
		"mtp:", "  file: m.gguf", "  sha256: "+strings.Repeat("b", 64), "  size: 2",
		"dflash:", "  file: d.gguf", "  sha256: "+strings.Repeat("c", 64), "  size: 3")
	_, err := Parse([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "both mtp and dflash") {
		t.Fatalf("want an error naming both draft kinds, got %v", err)
	}
}

func TestParseRefusesABadSiblingRepository(t *testing.T) {
	for _, repo := range []string{`"../x"`, "a/b/c", "a/b..c"} {
		body := "entries:\n" + entryYAML("a:1", "a.gguf",
			"dflash:", "  hf_repo: "+repo, "  file: d.gguf", "  sha256: "+strings.Repeat("c", 64), "  size: 3")
		_, err := Parse([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "owner/name") {
			t.Errorf("sibling hf_repo %s: want an owner/name error, got %v", repo, err)
		}
	}
}

func TestPresetAcceptsTheSpecDraftKeysAndRefusesTheRemovedOnes(t *testing.T) {
	ok := "entries:\n" + entryYAML("a:1", "a.gguf", "preset:", `  spec-draft-n-max: "6"`, `  spec-draft-n-min: "0"`,
		`  spec-draft-p-min: "0.5"`, `  draft-p-min: "0.5"`, `  spec-draft-ngl: "999"`, `  ngld: "999"`)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("the spec-draft keys were refused: %v", err)
	}
	for old, renamed := range map[string]string{"draft-max": "spec-draft-n-max", "draft-min": "spec-draft-n-min"} {
		body := "entries:\n" + entryYAML("a:1", "a.gguf", "preset:", "  "+old+`: "6"`)
		_, err := Parse([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "llama-server renamed it to "+renamed) {
			t.Errorf("%s, which llama-server removed, must be refused with its new name: %v", old, err)
		}
	}
}
