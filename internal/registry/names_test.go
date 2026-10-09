package registry

import (
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	good := []string{"gemma4:12b", "Qwen3.6-27B-Q4_K_M", "a", "glm-4.7-flash", strings.Repeat("x", 64)}
	for _, n := range good {
		if err := ValidName(n); err != nil {
			t.Errorf("%q should be valid: %v", n, err)
		}
	}
	bad := []string{"", "../x", "a/b", "-x", ".hidden", strings.Repeat("x", 65), "a..b", "\u00e4", "a b", "a\x00b"}
	for _, n := range bad {
		if err := ValidName(n); err == nil {
			t.Errorf("%q should be invalid", n)
		}
	}
}

func TestSectionIDMapsEveryColonToAHyphen(t *testing.T) {
	cases := map[string]string{"gemma4:12b": "gemma4-12b", "a:b:c": "a-b-c", "plain": "plain", "x-y": "x-y"}
	for name, want := range cases {
		if got := SectionID(name); got != want {
			t.Errorf("SectionID(%q) = %q, want %q", name, got, want)
		}
	}
}
