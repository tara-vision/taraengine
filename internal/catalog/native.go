package catalog

import (
	"fmt"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/gguf"
)

// ReadNativeContext is the context window the model in the GGUF file at path was trained for, as its header says.
// It is 0, with the reason, when the header cannot be read or names no window, and when the window it names lies
// outside the windows a load accepts: a native window below them would make every load at it fail.
func ReadNativeContext(path string) (int, error) {
	info, err := gguf.ReadInfo(path)
	if err != nil {
		return 0, err
	}
	n := info.ContextLength
	if n < config.MinContext || n > config.MaxContext {
		return 0, fmt.Errorf("catalog: the header gives a context length of %d, outside %d to %d", n,
			config.MinContext, config.MaxContext)
	}
	return int(n), nil
}
