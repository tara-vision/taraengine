package pull

import (
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		in   string
		want Ref
	}{
		{"gemma4:12b", Ref{Kind: KindRegistry, Name: "gemma4:12b"}},
		{"hf://org/repo/file.gguf", Ref{Kind: KindHF, Name: "file", Repo: "org/repo", File: "file.gguf"}},
		{"hf://ggml-org/models-moved/tinyllamas/stories260K.gguf", Ref{Kind: KindHF, Name: "stories260K", Repo: "ggml-org/models-moved", File: "tinyllamas/stories260K.gguf"}},
		{"/models/My-Model.gguf", Ref{Kind: KindLocal, Name: "My-Model", Path: "/models/My-Model.gguf"}},
	}
	for _, tc := range cases {
		got, err := ParseRef(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q: got %+v, want %+v", tc.in, got, tc.want)
		}
	}
	bad := []string{"", "hf://org", "hf://org/repo", "hf://org/repo/", "hf://org/repo/file.bin", "hf://org/repo/../x.gguf",
		"/models/weights.bin", "/models/bad name.gguf", "../x", "a/b"}
	for _, in := range bad {
		if _, err := ParseRef(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestParseRefRejectsBadHFOwnersAndFileNames(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hf://a b/c/file.gguf", "must be owner/name"},
		{"hf://org/repo/bad name.gguf", "model name"},
	}
	for _, tc := range cases {
		_, err := ParseRef(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: want error containing %q, got %v", tc.in, tc.want, err)
		}
	}
}
