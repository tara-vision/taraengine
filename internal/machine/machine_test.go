package machine

import (
	"context"
	"errors"
	goruntime "runtime"
	"testing"
)

func TestTierFor(t *testing.T) {
	cases := map[int64]string{
		8 << 30:  "small",
		16 << 30: "16",
		24 << 30: "16",
		32 << 30: "32",
		36 << 30: "32",
		48 << 30: "48",
		64 << 30: "48",
		0:        "small",
	}
	for ram, want := range cases {
		if got := TierFor(ram); got != want {
			t.Errorf("TierFor(%d) = %q, want %q", ram, got, want)
		}
	}
}

func TestTierForBoundaries(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		ram  int64
		want string
	}{
		{14*gib - 1, "small"},
		{14 * gib, "16"},
		{30*gib - 1, "16"},
		{30 * gib, "32"},
		{46*gib - 1, "32"},
		{46 * gib, "48"},
	}
	for _, tc := range cases {
		if got := TierFor(tc.ram); got != tc.want {
			t.Errorf("TierFor(%d) = %q, want %q", tc.ram, got, tc.want)
		}
	}
}

func TestDetectLinuxWithNvidia(t *testing.T) {
	run := func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "nvidia-smi" {
			return "NVIDIA GeForce RTX 5090, 32607, 580.65.06\n", nil
		}
		return "", errors.New("unexpected " + name)
	}
	readFile := func(string) ([]byte, error) { return []byte("MemTotal:       49280000 kB\nMemFree: 1 kB\n"), nil }
	info := Detect(context.Background(), "linux", run, readFile)
	if info.OS != "linux" || info.GPU != "NVIDIA GeForce RTX 5090" || info.GPUMemoryBytes != 32607<<20 || info.DriverVersion != "580.65.06" {
		t.Fatalf("gpu: %+v", info)
	}
	if info.RAMBytes != 49280000<<10 || info.Tier != "48" || info.UnifiedMemory {
		t.Fatalf("ram: %+v", info)
	}
}

func TestDetectLinuxWithoutNvidia(t *testing.T) {
	run := func(context.Context, string, ...string) (string, error) { return "", errors.New("absent") }
	readFile := func(string) ([]byte, error) { return []byte("MemTotal: 16000000 kB\n"), nil }
	info := Detect(context.Background(), "linux", run, readFile)
	if info.GPU != "" || info.GPUMemoryBytes != 0 || info.Tier != "16" {
		t.Fatalf("%+v", info)
	}
	broken := func(string) ([]byte, error) { return nil, errors.New("no meminfo") }
	if info := Detect(context.Background(), "linux", run, broken); info.RAMBytes != 0 || info.Tier != "small" {
		t.Fatalf("without meminfo: %+v", info)
	}
}

func TestDetectLinuxRAMEdgeCases(t *testing.T) {
	run := func(context.Context, string, ...string) (string, error) { return "", errors.New("absent") }
	cases := []struct {
		name     string
		readFile func(string) ([]byte, error)
		want     int64
	}{
		{"no file reader", nil, 0},
		{"MemTotal after other lines", fileReturning("MemFree: 1 kB\nMemTotal: 16000000 kB\n"), 16000000 << 10},
		{"no MemTotal line", fileReturning("MemFree: 1 kB\n"), 0},
		{"MemTotal without a value", fileReturning("MemTotal:\n"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := Detect(context.Background(), "linux", run, tc.readFile)
			if info.RAMBytes != tc.want {
				t.Fatalf("RAMBytes = %d, want %d", info.RAMBytes, tc.want)
			}
		})
	}
}

func fileReturning(content string) func(string) ([]byte, error) {
	return func(string) ([]byte, error) { return []byte(content), nil }
}

func TestDetectNvidiaOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
		gpu    string
		memory int64
		driver string
	}{
		{"first of several GPUs", "NVIDIA A, 1024, 1\nNVIDIA B, 2048, 2\n", "NVIDIA A", 1024 << 20, "1"},
		{"memory not reported", "NVIDIA GeForce RTX 5090, [N/A], 580.65.06\n", "NVIDIA GeForce RTX 5090", 0, "580.65.06"},
		{"too few fields", "NVIDIA GeForce RTX 5090\n", "", 0, ""},
		{"too many fields", "NVIDIA GeForce RTX 5090, 32607, 580.65.06, extra\n", "", 0, ""},
		{"empty output", "", "", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := Detect(context.Background(), "linux", nvidiaOnly(tc.output), nil)
			if info.GPU != tc.gpu || info.GPUMemoryBytes != tc.memory || info.DriverVersion != tc.driver {
				t.Fatalf("gpu %q memory %d driver %q, want %q %d %q", info.GPU, info.GPUMemoryBytes, info.DriverVersion, tc.gpu, tc.memory, tc.driver)
			}
		})
	}
}

func nvidiaOnly(output string) Runner {
	return func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "nvidia-smi" {
			return output, nil
		}
		return "", errors.New("unexpected " + name)
	}
}

func TestDetectDarwin(t *testing.T) {
	run := func(_ context.Context, name string, _ ...string) (string, error) {
		switch name {
		case "sysctl":
			return "51539607552\n", nil
		case "system_profiler":
			return `{"SPDisplaysDataType":[{"sppci_model":"Apple M4 Max","spdisplays_vendor":"sppci_vendor_Apple"}]}`, nil
		}
		return "", errors.New("unexpected " + name)
	}
	info := Detect(context.Background(), "darwin", run, nil)
	if info.GPU != "Apple M4 Max" || !info.UnifiedMemory || info.RAMBytes != 51539607552 || info.GPUMemoryBytes != 51539607552 || info.Tier != "48" {
		t.Fatalf("%+v", info)
	}
	failing := func(context.Context, string, ...string) (string, error) { return "", errors.New("x") }
	if info := Detect(context.Background(), "darwin", failing, nil); info.GPU != "Apple Silicon" || info.Tier != "small" {
		t.Fatalf("darwin without probes: %+v", info)
	}
}

func TestDetectDarwinGPUFallbacks(t *testing.T) {
	cases := []struct {
		name    string
		display string
	}{
		{"unparseable JSON", "not json"},
		{"no displays", `{"SPDisplaysDataType":[]}`},
		{"display without a model", `{"SPDisplaysDataType":[{"spdisplays_vendor":"sppci_vendor_Apple"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(_ context.Context, name string, _ ...string) (string, error) {
				switch name {
				case "sysctl":
					return "51539607552\n", nil
				case "system_profiler":
					return tc.display, nil
				}
				return "", errors.New("unexpected " + name)
			}
			info := Detect(context.Background(), "darwin", run, nil)
			if info.GPU != "Apple Silicon" || info.RAMBytes != 51539607552 {
				t.Fatalf("%+v", info)
			}
		})
	}
}

func TestExecRunner(t *testing.T) {
	out, err := ExecRunner(context.Background(), "echo", "hello")
	if err != nil || out != "hello\n" {
		t.Fatalf("echo: %q, %v", out, err)
	}
	if _, err := ExecRunner(context.Background(), "taraengine-no-such-command-xyz"); err == nil {
		t.Fatal("a missing command should fail")
	}
}

func TestDetectSetsArch(t *testing.T) {
	run := func(context.Context, string, ...string) (string, error) { return "", errors.New("absent") }
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			info := Detect(context.Background(), goos, run, nil)
			if info.Arch != goruntime.GOARCH {
				t.Fatalf("Arch = %q, want %q", info.Arch, goruntime.GOARCH)
			}
		})
	}
}
