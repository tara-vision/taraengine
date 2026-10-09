package measure

import (
	"reflect"
	"sort"
	"testing"
)

func TestDescendants(t *testing.T) {
	ps := "    1     0\n  100     1\n  200   100\n  201   100\n  300   200\n  400     1\n"
	got := Descendants(ps, 100)
	sort.Ints(got)
	if !reflect.DeepEqual(got, []int{100, 200, 201, 300}) {
		t.Fatalf("descendants %v", got)
	}
	if got := Descendants(ps, 999); !reflect.DeepEqual(got, []int{999}) {
		t.Fatalf("unknown root must still include itself: %v", got)
	}
	if got := Descendants("garbage\n", 1); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("garbage input: %v", got)
	}
}

func TestDescendantsSkipsNonNumericFields(t *testing.T) {
	ps := "  x     1\n  100     y\n  200   100\n"
	if got := Descendants(ps, 100); !reflect.DeepEqual(got, []int{100, 200}) {
		t.Fatalf("non-numeric pid or ppid must be skipped: %v", got)
	}
}

func TestDescendantsTerminatesOnCycles(t *testing.T) {
	cycle := "100 200\n200 100\n"
	if got := Descendants(cycle, 100); !reflect.DeepEqual(got, []int{100, 200}) {
		t.Fatalf("two-cycle through the root: %v", got)
	}
	self := "100 100\n"
	if got := Descendants(self, 100); !reflect.DeepEqual(got, []int{100}) {
		t.Fatalf("self-parent root: %v", got)
	}
}
