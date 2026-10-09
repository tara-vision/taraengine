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
			[]string{`unknown key "sha"`, "line 11", "accepted keys: file, sha256, size"},
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
