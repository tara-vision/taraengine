package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Platform is what the lock is keyed by: the OS, the architecture and the accelerator build.
type Platform struct {
	OS    string
	Arch  string
	Accel string // metal, cuda13, cuda12, cpu
}

// Key is the lock key: darwin/arm64 on macOS (Metal is the only build), os/arch/accel elsewhere.
func (p Platform) Key() string {
	if p.OS == "darwin" {
		return p.OS + "/" + p.Arch
	}
	return p.OS + "/" + p.Arch + "/" + p.Accel
}

// CUDAMajor is the CUDA major version the build links against: 13 for cuda13, 12 for cuda12, and 0
// for builds with no CUDA requirement.
func (p Platform) CUDAMajor() int {
	switch p.Accel {
	case "cuda13":
		return 13
	case "cuda12":
		return 12
	}
	return 0
}

// Select picks the build for this machine. Linux takes the CUDA 13.4 build from driver 580, the
// CUDA 12.8 build from driver 570 (amd64 only, upstream ships no arm64 12.8 build), else the CPU
// build. Apple Silicon takes the Metal build. Anything else is not supported in this release.
func Select(goos, goarch, driverVersion string) (Platform, error) {
	switch {
	case goos == "darwin" && goarch == "arm64":
		return Platform{OS: goos, Arch: goarch, Accel: "metal"}, nil
	case goos == "linux" && (goarch == "amd64" || goarch == "arm64"):
		return Platform{OS: goos, Arch: goarch, Accel: cudaAccel(goarch, driverMajor(driverVersion))}, nil
	}
	return Platform{}, fmt.Errorf("runtime: %s/%s is not supported in this release (Linux with NVIDIA, or Apple Silicon)", goos, goarch) //nolint:lll // one error literal, kept whole for grep
}

func cudaAccel(goarch string, major int) string {
	switch {
	case major >= 580:
		return "cuda13"
	case major >= 570 && goarch == "amd64":
		return "cuda12"
	}
	return "cpu"
}

func driverMajor(v string) int {
	head, _, _ := strings.Cut(strings.TrimSpace(v), ".")
	n, err := strconv.Atoi(head)
	if err != nil {
		return 0
	}
	return n
}

// Runner runs a command and returns its standard output; tests inject one.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner is the Runner that runs real commands.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// DriverVersion is the NVIDIA driver version from nvidia-smi, or "" when there is none.
func DriverVersion(ctx context.Context, run Runner) string {
	out, err := run(ctx, "nvidia-smi", "--query-gpu=driver_version", "--format=csv,noheader")
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(first)
}

// HasCUDARuntime reports whether ldconfig lists libcudart.so.<major>, in which case the CUDA runtime
// bundle is not needed. A major of 0 means the build needs no CUDA runtime, so it reports false
// without running ldconfig.
func HasCUDARuntime(ctx context.Context, run Runner, major int) bool {
	if major == 0 {
		return false
	}
	out, err := run(ctx, "ldconfig", "-p")
	if err != nil {
		return false
	}
	want := "libcudart.so." + strconv.Itoa(major)
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == want {
			return true
		}
	}
	return false
}
