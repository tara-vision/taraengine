package catalog

import (
	"fmt"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/gguf"
)

// Header is what the engine keeps of a GGUF header: the context window the model was trained for, 0 when the header
// gives none or one outside the windows a load accepts, and the quantization its file type names, "" when unknown.
type Header struct {
	NativeContext int
	Quantization  string
}

// ReadHeader reads the header of the GGUF file at path. The error says why the window is 0 when it is: a header that
// cannot be read or names no window, or a window outside the windows a load accepts (a native window below them would
// make every load at it fail); the quantization is what the header gave either way.
func ReadHeader(path string) (Header, error) {
	info, err := gguf.ReadInfo(path)
	if err != nil {
		return Header{}, err
	}
	h := Header{Quantization: info.Quantization()}
	n := info.ContextLength
	if n < config.MinContext || n > config.MaxContext {
		return h, fmt.Errorf("catalog: the header gives a context length of %d, outside %d to %d", n,
			config.MinContext, config.MaxContext)
	}
	h.NativeContext = int(n)
	return h, nil
}

// ReadNativeContext is the context window the model in the GGUF file at path was trained for, as its header says;
// 0 with the reason when ReadHeader gives none.
func ReadNativeContext(path string) (int, error) {
	h, err := ReadHeader(path)
	return h.NativeContext, err
}
