package llamaserver

import (
	"slices"
	"testing"
)

func TestEditDistance(t *testing.T) {
	rows := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"gemma4:12b", "gemma4:12b", 0},
		{"gemma4:12", "gemma4:12b", 1},
		{"gemma4:21b", "gemma4:12b", 2},
		{"kitten", "sitting", 3},
	}
	for _, row := range rows {
		if got := editDistance(row.a, row.b); got != row.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", row.a, row.b, got, row.want)
		}
	}
}

func TestNearestNames(t *testing.T) {
	names := []string{"qwen3.6:27b", "gemma4:e4b", "gemma4:12b", "gemma4:26b"}
	if got := nearestNames("gemma4:12", names, 3); !slices.Equal(got, []string{"gemma4:12b", "gemma4:26b", "gemma4:e4b"}) {
		t.Errorf("nearest first, name order between equals: %v", got)
	}
	if got := nearestNames("QWEN3.6:27B", names, 1); !slices.Equal(got, []string{"qwen3.6:27b"}) {
		t.Errorf("case is ignored: %v", got)
	}
	if got := nearestNames("x", names[:2], 3); len(got) != 2 {
		t.Errorf("never more names than there are: %v", got)
	}
	if got := nearestNames("x", nil, 3); len(got) != 0 {
		t.Errorf("no names, no suggestions: %v", got)
	}
}
