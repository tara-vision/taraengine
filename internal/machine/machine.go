package machine

import (
	"context"
	"encoding/json"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"
)

// Info is what doctor and the API report about the host. GPU, GPUMemoryBytes and DriverVersion describe
// the first GPU nvidia-smi lists; a host with more than one reports only that one.
type Info struct {
	OS             string
	Arch           string
	GPU            string
	GPUMemoryBytes int64
	UnifiedMemory  bool
	RAMBytes       int64
	Tier           string
	DriverVersion  string
}

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs real commands.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// Detect reads the host through the injected runner and file reader. A failing probe never returns an
// error: its figures (RAM, GPU memory, driver version) stay empty, so doctor always has something to
// show. On darwin the GPU name falls back to "Apple Silicon", which is true for every Mac the engine
// supports. Arch is the architecture this binary was built for, from the Go runtime.
func Detect(ctx context.Context, goos string, run Runner, readFile func(string) ([]byte, error)) Info {
	info := Info{OS: goos, Arch: goruntime.GOARCH}
	switch goos {
	case "darwin":
		info.UnifiedMemory = true
		info.RAMBytes = darwinRAM(ctx, run)
		info.GPUMemoryBytes = info.RAMBytes
		info.GPU = darwinGPU(ctx, run)
	case "linux":
		info.RAMBytes = linuxRAM(readFile)
		info.GPU, info.GPUMemoryBytes, info.DriverVersion = nvidia(ctx, run)
	}
	info.Tier = TierFor(info.RAMBytes)
	return info
}

func darwinRAM(ctx context.Context, run Runner) int64 {
	out, err := run(ctx, "sysctl", "-n", "hw.memsize")
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return n
}

func darwinGPU(ctx context.Context, run Runner) string {
	out, err := run(ctx, "system_profiler", "SPDisplaysDataType", "-json")
	if err != nil {
		return "Apple Silicon"
	}
	var doc struct {
		Displays []struct {
			Model string `json:"sppci_model"`
		} `json:"SPDisplaysDataType"`
	}
	if json.Unmarshal([]byte(out), &doc) != nil || len(doc.Displays) == 0 || doc.Displays[0].Model == "" {
		return "Apple Silicon"
	}
	return doc.Displays[0].Model
}

func linuxRAM(readFile func(string) ([]byte, error)) int64 {
	if readFile == nil {
		return 0
	}
	data, err := readFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, _ := strconv.ParseInt(fields[1], 10, 64)
		return kb << 10
	}
	return 0
}

// nvidia reads name, memory (MiB) and driver version of the first GPU nvidia-smi lists.
func nvidia(ctx context.Context, run Runner) (name string, memory int64, driver string) {
	out, err := run(ctx, "nvidia-smi", "--query-gpu=name,memory.total,driver_version", "--format=csv,noheader,nounits")
	if err != nil {
		return "", 0, ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	parts := strings.Split(first, ",")
	if len(parts) != 3 {
		return "", 0, ""
	}
	mib, _ := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	return strings.TrimSpace(parts[0]), mib << 20, strings.TrimSpace(parts[2])
}
