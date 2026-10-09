package docscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoticeKeepsTheLicenseOfLlamaCpp pins what spec section 9 asks of the NOTICE: it names llama.cpp and ggml-org and
// keeps their license text, the MIT License with the copyright line of the ggml authors, in full.
func TestNoticeKeepsTheLicenseOfLlamaCpp(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "NOTICE"))
	if err != nil {
		t.Fatal(err)
	}
	notice := string(data)
	for _, want := range []string{
		"https://github.com/ggml-org/llama.cpp",
		"MIT License\n\nCopyright (c) 2023-2026 The ggml authors\n\nPermission is hereby granted, free of charge,",
		"copies of the Software, and to permit persons to whom the Software is\nfurnished to do so",
		"The above copyright notice and this permission notice shall be included in all\ncopies or substantial portions of the Software.",
		"THE SOFTWARE IS PROVIDED \"AS IS\", WITHOUT WARRANTY OF ANY KIND",
		"OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE\nSOFTWARE.",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("the NOTICE lacks %q", want)
		}
	}
}
