package cli

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// size formats bytes the way download pages do, in MB with one decimal below a gigabyte and in GB from there, so
// that a file of a few megabytes is not shown as 0.0 GB. The figure is rounded first and the unit chosen after, so
// that 999.96 MB reads 1.0 GB and never 1000.0 MB.
func size(n int64) string {
	if mb := math.Round(float64(n)/1e5) / 10; mb < 1000 {
		return fmt.Sprintf("%.1f MB", mb)
	}
	return fmt.Sprintf("%.1f GB", float64(n)/1e9)
}

// gib formats bytes as GiB with one decimal, the unit nvidia-smi uses.
func gib(n int64) string { return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30)) }

// buildNumber is the number of an upstream build tag (b11514 -> 11514), 0 when it is not one.
func buildNumber(build string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(build, "b"))
	if err != nil {
		return 0
	}
	return n
}

// printer writes the lines of a command's output. It keeps the first write error and writes nothing after it, so
// that a command can print freely and return the error once at the end.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}
