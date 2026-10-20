package llamaserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPresetsIsDeterministic(t *testing.T) {
	models := []PresetModel{
		{Name: "qwen3.6:27b", ModelPath: "/m/q.gguf", MMProjPath: "/m/mm.gguf", DraftPath: "/m/mtp.gguf", Context: 32768, Extra: map[string]string{"cache-type-k": "q8_0", "c": "16384"}},
		{Name: "glm-4.7-flash", ModelPath: "/m/glm.gguf", Context: 32768},
	}
	got := RenderPresets(DefaultGlobal(), models)
	want := `[*]
flash-attn = auto
jinja = true
load-on-startup = false
n-gpu-layers = 999
reasoning-format = deepseek

[qwen3.6-27b]
c = 32768
cache-type-k = q8_0
mmproj = /m/mm.gguf
model = /m/q.gguf
model-draft = /m/mtp.gguf
spec-type = draft-mtp

[glm-4.7-flash]
c = 32768
model = /m/glm.gguf
`
	if got != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", got, want)
	}
	if again := RenderPresets(DefaultGlobal(), models); again != got {
		t.Fatal("rendering is not deterministic")
	}
}

func TestRenderPresetsEngineKeysWinOverExtras(t *testing.T) {
	models := []PresetModel{{
		Name:       "gemma4:12b",
		ModelPath:  "/m/real.gguf",
		MMProjPath: "/m/real-mm.gguf",
		DraftPath:  "/m/real-draft.gguf",
		Context:    4096,
		Extra: map[string]string{
			"model":        "/m/other.gguf",
			"ctx-size":     "1024",
			"mmproj":       "/m/other-mm.gguf",
			"model-draft":  "/m/other-draft.gguf",
			"spec-type":    "ngram-simple",
			"n-gpu-layers": "0",
		},
	}}
	got := RenderPresets(DefaultGlobal(), models)
	want := `[*]
flash-attn = auto
jinja = true
load-on-startup = false
n-gpu-layers = 999
reasoning-format = deepseek

[gemma4-12b]
c = 4096
mmproj = /m/real-mm.gguf
model = /m/real.gguf
model-draft = /m/real-draft.gguf
n-gpu-layers = 0
spec-type = draft-mtp
`
	if got != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderPresetsOmitsContextWhenZero(t *testing.T) {
	got := RenderPresets(DefaultGlobal(), []PresetModel{{Name: "glm-4.7-flash", ModelPath: "/m/glm.gguf"}})
	want := `[*]
flash-attn = auto
jinja = true
load-on-startup = false
n-gpu-layers = 999
reasoning-format = deepseek

[glm-4.7-flash]
model = /m/glm.gguf
`
	if got != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderPresetsGlobalOverrides(t *testing.T) {
	global := DefaultGlobal()
	global["n-gpu-layers"] = "0"
	global["cache-type-v"] = "q8_0"
	got := RenderPresets(global, nil)
	for _, line := range []string{"n-gpu-layers = 0\n", "cache-type-v = q8_0\n"} {
		if !contains(got, line) {
			t.Errorf("missing %q in:\n%s", line, got)
		}
	}
	if contains(got, "n-gpu-layers = 999") {
		t.Fatal("override lost")
	}
}

func TestWritePresetsIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.ini")
	if err := WritePresets(path, "[*]\njinja = true\n"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "[*]\njinja = true\n" {
		t.Fatalf("content %q", data)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
	if err := WritePresets(filepath.Join(t.TempDir(), "missing", "dir", "presets.ini"), "x"); err == nil {
		t.Fatal("missing directory must fail")
	}
}

func TestWritePresetsFailsWhenPathIsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "presets.ini")
	if err := os.Mkdir(path, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := WritePresets(path, "x"); err == nil {
		t.Fatal("rename onto a directory must fail")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind after a failed rename")
	}
}

func TestSectionID(t *testing.T) {
	if got := SectionID("gemma4:12b"); got != "gemma4-12b" {
		t.Fatalf("section id %q", got)
	}
	if got := SectionID("glm-4.7-flash"); got != "glm-4.7-flash" {
		t.Fatalf("section id %q", got)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRenderPresetsWritesTheDraftKind(t *testing.T) {
	cases := map[string]string{"dflash": "draft-dflash", "mtp": "draft-mtp", "": "draft-mtp"}
	for kind, want := range cases {
		got := RenderPresets(map[string]string{}, []PresetModel{{Name: "a:1", ModelPath: "/m/a.gguf", DraftPath: "/m/d.gguf", DraftKind: kind}})
		if !strings.Contains(got, "spec-type = "+want+"\n") {
			t.Errorf("draft kind %q rendered:\n%s\nwant spec-type = %s", kind, got, want)
		}
	}
	got := RenderPresets(map[string]string{}, []PresetModel{{Name: "a:1", ModelPath: "/m/a.gguf", DraftKind: "dflash"}})
	if strings.Contains(got, "spec-type") {
		t.Errorf("a draft kind without a draft path rendered spec-type:\n%s", got)
	}
}
