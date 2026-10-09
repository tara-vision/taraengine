package measure

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/tara-vision/taraengine/internal/backend"
)

// Runner runs a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs real commands.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// ErrNoFigure means no process of the tree holds memory the probe can see.
var ErrNoFigure = errors.New("measure: no process of the tree holds memory the probe can see")

// Probe measures the memory held by a process tree.
type Probe interface {
	Measure(ctx context.Context, rootPID int) (backend.Memory, error)
}

// Default is the NVIDIA probe on Linux when nvidia-smi answers, else the resident-memory probe.
func Default(goos string, run Runner) Probe {
	if goos == "linux" {
		if _, err := run(context.Background(), "nvidia-smi", "-L"); err == nil {
			return NvidiaProbe{Run: run}
		}
	}
	return RSSProbe{Run: run}
}

func tree(ctx context.Context, run Runner, root int) ([]int, error) {
	if root <= 0 {
		return nil, errors.New("measure: root pid must be positive")
	}
	out, err := run(ctx, "ps", "-axo", "pid=,ppid=")
	if err != nil {
		return nil, fmt.Errorf("measure: ps: %w", err)
	}
	return Descendants(out, root), nil
}

// NvidiaProbe sums the GPU memory nvidia-smi reports for the processes of the tree, in MiB.
type NvidiaProbe struct {
	Run Runner
}

// Measure implements Probe with a measured figure.
func (p NvidiaProbe) Measure(ctx context.Context, rootPID int) (backend.Memory, error) {
	pids, err := tree(ctx, p.Run, rootPID)
	if err != nil {
		return backend.Memory{}, err
	}
	out, err := p.Run(ctx, "nvidia-smi", "--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits")
	if err != nil {
		return backend.Memory{}, fmt.Errorf("measure: nvidia-smi: %w", err)
	}
	inTree := map[int]bool{}
	for _, pid := range pids {
		inTree[pid] = true
	}
	var total int64
	for _, line := range strings.Split(out, "\n") {
		pidText, memText, ok := strings.Cut(line, ",")
		if !ok {
			continue
		}
		pid, err1 := strconv.Atoi(strings.TrimSpace(pidText))
		mib, err2 := strconv.ParseInt(strings.TrimSpace(memText), 10, 64)
		if err1 != nil || err2 != nil || !inTree[pid] {
			continue
		}
		total += mib << 20
	}
	if total == 0 {
		return backend.Memory{}, ErrNoFigure
	}
	return backend.Memory{Bytes: total, Source: "measured"}, nil
}

// RSSProbe sums the resident set of the tree, in KiB from ps. On Apple Silicon the weights are
// mapped into unified memory, so this is an estimate and is labeled as one.
type RSSProbe struct {
	Run Runner
}

// Measure implements Probe with an estimated figure.
func (p RSSProbe) Measure(ctx context.Context, rootPID int) (backend.Memory, error) {
	pids, err := tree(ctx, p.Run, rootPID)
	if err != nil {
		return backend.Memory{}, err
	}
	list := make([]string, 0, len(pids))
	for _, pid := range pids {
		list = append(list, strconv.Itoa(pid))
	}
	out, err := p.Run(ctx, "ps", "-o", "rss=", "-p", strings.Join(list, ","))
	if err != nil {
		return backend.Memory{}, fmt.Errorf("measure: ps rss: %w", err)
	}
	var total int64
	for _, line := range strings.Split(out, "\n") {
		kib, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
		if err == nil {
			total += kib << 10
		}
	}
	if total == 0 {
		return backend.Memory{}, ErrNoFigure
	}
	return backend.Memory{Bytes: total, Source: "estimated"}, nil
}
