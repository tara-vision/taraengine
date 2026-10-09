package docscheck

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/registry"
)

// tableHeader is the header of the catalog table in the README.
var tableHeader = []string{"Name", "Tier", "Size", "Capabilities"}

// cells splits a markdown table row into its trimmed cells, or returns nil for a line that is not a row.
func cells(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	parts := strings.Split(strings.Trim(line, "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readmeRows returns the rows of the catalog table of a README, without the header and the separator. It is nil when
// the README has no such table.
func readmeRows(readme string) [][]string {
	lines := strings.Split(readme, "\n")
	for i, line := range lines {
		if !equal(cells(line), tableHeader) {
			continue
		}
		var rows [][]string
		for _, next := range lines[i+2:] { // the line after the header is the separator
			row := cells(next)
			if row == nil {
				break
			}
			rows = append(rows, row)
		}
		return rows
	}
	return nil
}

// expectedRow is how the README shows a registry entry: the name, the RAM tier with a mark for the tier's default,
// the size of the main file in decimal gigabytes as `taraengine list` prints it, and the capabilities.
func expectedRow(e registry.Entry) []string {
	tier := e.Tier
	if e.Default {
		tier += " (default)"
	}
	return []string{e.Name, tier, fmt.Sprintf("%.1f GB", float64(e.Size)/1e9), strings.Join(e.Capabilities, ", ")}
}

// mismatches compares the catalog table of a README with the registry entries, row by row, and describes every
// difference. An empty result means the README follows the registry.
func mismatches(readme string, entries []registry.Entry) []string {
	rows := readmeRows(readme)
	if rows == nil {
		return []string{"the README has no table with the header " + strings.Join(tableHeader, " | ")}
	}
	var out []string
	for i, e := range entries {
		want := expectedRow(e)
		switch {
		case i >= len(rows):
			out = append(out, fmt.Sprintf("row %d: the README lacks %s, want %v", i+1, e.Name, want))
		case !equal(rows[i], want):
			out = append(out, fmt.Sprintf("row %d: the README has %v, the registry says %v", i+1, rows[i], want))
		}
	}
	for i := len(entries); i < len(rows); i++ {
		out = append(out, fmt.Sprintf("row %d: the README lists %v, which the registry does not have", i+1, rows[i]))
	}
	return out
}

func TestReadmeCatalogTableFollowsTheEmbeddedRegistry(t *testing.T) {
	reg, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range mismatches(string(readme), reg.Entries) {
		t.Error(problem)
	}
}

func TestMismatchesDescribeEveryDifference(t *testing.T) {
	entries := []registry.Entry{
		{Name: "a:1b", Tier: "small", Default: true, Size: 1_250_000_000, Capabilities: []string{"tools", "vision"}},
		{Name: "b:2b", Tier: "16", Size: 2_000_000_000, Capabilities: []string{"tools"}},
	}
	table := func(rows ...string) string {
		return "intro\n\n| Name | Tier | Size | Capabilities |\n|------|------|------|--------------|\n" +
			strings.Join(rows, "\n") + "\n\nafter\n"
	}
	good := table("| a:1b | small (default) | 1.2 GB | tools, vision |", "| b:2b | 16 | 2.0 GB | tools |")
	cases := []struct {
		name, readme string
		want         int
		contains     string
	}{
		{"in step", good, 0, ""},
		{"a wrong size", strings.Replace(good, "1.2 GB", "1.3 GB", 1), 1, "row 1"},
		{"a lost default mark", strings.Replace(good, "small (default)", "small", 1), 1, "row 1"},
		{"other capabilities", strings.Replace(good, "| tools |", "| tools, thinking |", 1), 1, "row 2"},
		{"a missing row", table("| a:1b | small (default) | 1.2 GB | tools, vision |"), 1, "lacks b:2b"},
		{"an extra row", table("| a:1b | small (default) | 1.2 GB | tools, vision |",
			"| b:2b | 16 | 2.0 GB | tools |", "| c:3b | 32 | 3.0 GB | tools |"), 1, "does not have"},
		{"no table", "nothing here\n", 1, "no table"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mismatches(tc.readme, entries)
			if len(got) != tc.want || (tc.want > 0 && !strings.Contains(got[0], tc.contains)) {
				t.Fatalf("got %d differences %v, want %d mentioning %q", len(got), got, tc.want, tc.contains)
			}
		})
	}
}
