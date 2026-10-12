package catalog

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/tara-vision/taraengine/internal/gguf/gguftest"
)

func TestReadNativeContextTakesOnlyTheWindowsALoadAccepts(t *testing.T) {
	dir := t.TempDir()
	arch := gguftest.String("general.architecture", "llama")
	rows := []struct {
		name   string
		header []byte
		want   int
	}{
		{"a window a load accepts", gguftest.Model("llama", 2048), 2048},
		{"the smallest window", gguftest.Model("llama", 512), 512},
		{"the largest window", gguftest.Header(3, arch, gguftest.U64("llama.context_length", 4194304)), 4194304},
		{"a window below the smallest", gguftest.Model("llama", 100), 0},
		{"no window at all", gguftest.Model("llama", 0), 0},
		{"a window above the largest", gguftest.Header(3, arch, gguftest.U64("llama.context_length", 4194305)), 0},
		{"a window no int holds", gguftest.Header(3, arch, gguftest.U64("llama.context_length", 1<<63)), 0},
		{"no header", []byte("x"), 0},
	}
	for i, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			path := filepath.Join(dir, strconv.Itoa(i)+".gguf")
			if err := os.WriteFile(path, row.header, 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ReadNativeContext(path)
			if got != row.want || (err == nil) != (row.want > 0) {
				t.Fatalf("got %d, %v; want %d", got, err, row.want)
			}
		})
	}
}
