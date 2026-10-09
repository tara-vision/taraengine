package llamaserver

import (
	"slices"
	"testing"
)

func TestChildEnvironmentKeepsOnlyWhatLlamaServerNeeds(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin", "HOME=/home/u", "TMPDIR=/tmp/u", "LANG=C", "LC_ALL=C", "LC_CTYPE=UTF-8",
		"CUDA_VISIBLE_DEVICES=1", "CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"TARAENGINE_API_KEY=s3cret", "TARAENGINE_HF_TOKEN=hf_a", "HF_TOKEN=hf_b", "HUGGING_FACE_HUB_TOKEN=hf_c",
		"LLAMA_ARG_HOST=0.0.0.0", "LLAMA_CACHE=/home/u/.cache/llama.cpp", "HF_HOME=/home/u/.cache/huggingface",
		"LD_LIBRARY_PATH=/usr/local/cuda/lib64", "DYLD_LIBRARY_PATH=", "USER=u", "SHELL=/bin/zsh",
		"NO_EQUALS_SIGN", "=hidden",
	}
	got := childEnvironment(environ, "/rt", "/data/llama-cache")
	want := []string{
		"PATH=/usr/bin", "HOME=/home/u", "TMPDIR=/tmp/u", "LANG=C", "LC_ALL=C", "LC_CTYPE=UTF-8",
		"CUDA_VISIBLE_DEVICES=1", "CUDA_DEVICE_ORDER=PCI_BUS_ID",
		"LD_LIBRARY_PATH=/rt:/usr/local/cuda/lib64", "DYLD_LIBRARY_PATH=/rt", "LLAMA_CACHE=/data/llama-cache",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("environment\n got %v\nwant %v", got, want)
	}
}
