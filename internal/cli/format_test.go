package cli

import (
	"bytes"
	"errors"
	"testing"
)

func TestSizeFormats(t *testing.T) {
	for _, c := range []struct {
		bytes     int64
		size, gib string
	}{
		{0, "0.0 MB", "0.0 GiB"},
		{16, "0.0 MB", "0.0 GiB"},
		{11_000_000, "11.0 MB", "0.0 GiB"},
		{999_949_999, "999.9 MB", "0.9 GiB"},
		{999_950_000, "1.0 GB", "0.9 GiB"},
		{999_999_999, "1.0 GB", "0.9 GiB"},
		{1_000_000_000, "1.0 GB", "0.9 GiB"},
		{7219673216, "7.2 GB", "6.7 GiB"},
		{10 << 30, "10.7 GB", "10.0 GiB"},
		{1_500_000_000, "1.5 GB", "1.4 GiB"},
	} {
		if got := size(c.bytes); got != c.size {
			t.Errorf("size(%d) = %q, want %q", c.bytes, got, c.size)
		}
		if got := gib(c.bytes); got != c.gib {
			t.Errorf("gib(%d) = %q, want %q", c.bytes, got, c.gib)
		}
	}
}

func TestBuildNumber(t *testing.T) {
	for in, want := range map[string]int{"b11514": 11514, "11514": 11514, "b1": 1, "": 0, "b": 0, "latest": 0, "b11x": 0, "bb5": 0} {
		if got := buildNumber(in); got != want {
			t.Errorf("buildNumber(%q) = %d, want %d", in, got, want)
		}
	}
}

// failingWriter refuses every write and counts them.
type failingWriter struct {
	err    error
	writes int
}

func (w *failingWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, w.err
}

func TestPrinterKeepsTheFirstWriteError(t *testing.T) {
	var out bytes.Buffer
	ok := &printer{w: &out}
	ok.printf("a %d\n", 1)
	ok.printf("b\n")
	if ok.err != nil || out.String() != "a 1\nb\n" {
		t.Fatalf("%v %q", ok.err, out.String())
	}
	boom := errors.New("disk full")
	w := &failingWriter{err: boom}
	bad := &printer{w: w}
	bad.printf("a\n")
	bad.printf("b\n")
	if !errors.Is(bad.err, boom) || w.writes != 1 {
		t.Fatalf("after the first failure nothing more is written: %v, %d writes", bad.err, w.writes)
	}
}
