package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestSelect(t *testing.T) {
	cases := []struct {
		goos, goarch, driver string
		want                 string
		fails                bool
	}{
		{"darwin", "arm64", "", "darwin/arm64", false},
		{"linux", "amd64", "580.65.06", "linux/amd64/cuda13", false},
		{"linux", "amd64", "575.57.08", "linux/amd64/cuda12", false},
		{"linux", "amd64", "570.133", "linux/amd64/cuda12", false},
		{"linux", "amd64", "560.35", "linux/amd64/cpu", false},
		{"linux", "amd64", "", "linux/amd64/cpu", false},
		{"linux", "amd64", "N/A", "linux/amd64/cpu", false},
		{"linux", "arm64", "580.1", "linux/arm64/cuda13", false},
		{"linux", "arm64", "570.1", "linux/arm64/cpu", false},
		{"darwin", "amd64", "", "", true},
		{"windows", "amd64", "580.1", "", true},
	}
	for _, tc := range cases {
		p, err := Select(tc.goos, tc.goarch, tc.driver)
		if tc.fails {
			if err == nil {
				t.Errorf("%s/%s should fail", tc.goos, tc.goarch)
			}
			continue
		}
		if err != nil || p.Key() != tc.want {
			t.Errorf("%s/%s driver %q: got %q (%v), want %q", tc.goos, tc.goarch, tc.driver, p.Key(), err, tc.want)
		}
	}
}

func TestDriverVersionAndCUDARuntime(t *testing.T) {
	run := func(_ context.Context, name string, _ ...string) (string, error) {
		switch name {
		case "nvidia-smi":
			return "580.65.06\n580.65.06\n", nil
		case "ldconfig":
			return "libcudart.so.12 (libc6,x86-64) => /usr/lib/libcudart.so.12\n", nil
		}
		return "", errors.New("unknown")
	}
	if got := DriverVersion(context.Background(), run); got != "580.65.06" {
		t.Fatalf("driver %q", got)
	}
	if !HasCUDARuntime(context.Background(), run, 12) {
		t.Fatal("cuda runtime not detected")
	}
	failing := func(context.Context, string, ...string) (string, error) { return "", errors.New("not installed") }
	if got := DriverVersion(context.Background(), failing); got != "" {
		t.Fatalf("driver without nvidia-smi %q", got)
	}
	if HasCUDARuntime(context.Background(), failing, 12) {
		t.Fatal("cuda runtime detected without ldconfig")
	}
}

func TestExecRunnerRunsACommand(t *testing.T) {
	out, err := ExecRunner(context.Background(), "echo", "hello")
	if err != nil || out != "hello\n" {
		t.Fatalf("got %q, %v", out, err)
	}
	if _, err := ExecRunner(context.Background(), "taraengine-no-such-command-xyz"); err == nil {
		t.Fatal("missing command must fail")
	}
}

func TestHasCUDARuntimeMatchesMajor(t *testing.T) {
	listing := func(out string) Runner {
		return func(context.Context, string, ...string) (string, error) { return out, nil }
	}
	bundle12 := listing("\tlibcudart.so.12 (libc6,x86-64) => /usr/lib/libcudart.so.12\n")
	if HasCUDARuntime(context.Background(), bundle12, 13) {
		t.Fatal("libcudart.so.12 satisfied a CUDA 13 build")
	}
	if !HasCUDARuntime(context.Background(), bundle12, 12) {
		t.Fatal("libcudart.so.12 not detected for a CUDA 12 build")
	}
	if HasCUDARuntime(context.Background(), listing("\tlibcudart.so.120 (libc6) => /usr/lib/libcudart.so.120\n"), 12) {
		t.Fatal("libcudart.so.120 matched a CUDA 12 build")
	}
}

func TestHasCUDARuntimeMajorZeroSkipsLdconfig(t *testing.T) {
	ran := false
	run := func(context.Context, string, ...string) (string, error) {
		ran = true
		return "\tlibcudart.so.12 (libc6) => /usr/lib/libcudart.so.12\n", nil
	}
	if HasCUDARuntime(context.Background(), run, 0) {
		t.Fatal("major 0 reported a CUDA runtime")
	}
	if ran {
		t.Fatal("ldconfig ran for a build with no CUDA requirement")
	}
}

func TestCUDAMajor(t *testing.T) {
	cases := []struct {
		accel string
		want  int
	}{
		{"cuda13", 13},
		{"cuda12", 12},
		{"cpu", 0},
		{"metal", 0},
	}
	for _, tc := range cases {
		if got := (Platform{Accel: tc.accel}).CUDAMajor(); got != tc.want {
			t.Errorf("accel %s: CUDAMajor %d, want %d", tc.accel, got, tc.want)
		}
	}
}
