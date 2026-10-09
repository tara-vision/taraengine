package measure

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const psTree = "  100     1\n  200   100\n  300   200\n  400     1\n"

func TestNvidiaProbeSumsTheProcessTree(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "ps":
			return psTree, nil
		case "nvidia-smi":
			if strings.Contains(strings.Join(args, " "), "compute-apps") {
				return "200, 19876\n300, 100\n400, 5000\n", nil
			}
		}
		return "", errors.New("unexpected " + name)
	}
	m, err := NvidiaProbe{Run: run}.Measure(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if m.Bytes != (19876+100)<<20 || m.Source != "measured" {
		t.Fatalf("memory %+v", m)
	}
	none := func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "ps" {
			return psTree, nil
		}
		return "400, 5000\n", nil
	}
	if _, err := (NvidiaProbe{Run: none}).Measure(context.Background(), 100); !errors.Is(err, ErrNoFigure) {
		t.Fatalf("no process of the tree on the GPU: %v", err)
	}
	failing := func(context.Context, string, ...string) (string, error) { return "", errors.New("no nvidia-smi") }
	if _, err := (NvidiaProbe{Run: failing}).Measure(context.Background(), 100); err == nil {
		t.Fatal("a failing nvidia-smi must fail the probe")
	}
}

func TestRSSProbeSumsTheProcessTree(t *testing.T) {
	var pids string
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "ps" && len(args) > 0 && args[0] == "-axo" {
			return psTree, nil
		}
		pids = args[len(args)-1]
		return " 1024\n 2048\n", nil
	}
	m, err := RSSProbe{Run: run}.Measure(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if m.Bytes != 3072*1024 || m.Source != "estimated" {
		t.Fatalf("memory %+v", m)
	}
	if pids != "100,200,300" {
		t.Fatalf("pids asked %q", pids)
	}
}

func TestDefaultPicksNvidiaOnLinuxWithADriver(t *testing.T) {
	withSMI := func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "nvidia-smi" {
			return "GPU 0: NVIDIA GeForce RTX 5090\n", nil
		}
		return "", nil
	}
	if _, ok := Default("linux", withSMI).(NvidiaProbe); !ok {
		t.Fatal("linux with nvidia-smi must use the NVIDIA probe")
	}
	noSMI := func(context.Context, string, ...string) (string, error) { return "", errors.New("absent") }
	if _, ok := Default("linux", noSMI).(RSSProbe); !ok {
		t.Fatal("linux without nvidia-smi must fall back to RSS")
	}
	if _, ok := Default("darwin", withSMI).(RSSProbe); !ok {
		t.Fatal("macOS must use RSS")
	}
}

func TestExecRunner(t *testing.T) {
	out, err := ExecRunner(context.Background(), "echo", "x")
	if err != nil || out != "x\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestNvidiaProbeFailsWhenNvidiaSMIFailsAfterPS(t *testing.T) {
	run := func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "ps" {
			return psTree, nil
		}
		return "", errors.New("no nvidia-smi")
	}
	if _, err := (NvidiaProbe{Run: run}).Measure(context.Background(), 100); err == nil {
		t.Fatal("a failing nvidia-smi query must fail the probe")
	}
}

func TestRSSProbeFailsWhenPSFails(t *testing.T) {
	failing := func(context.Context, string, ...string) (string, error) { return "", errors.New("no ps") }
	if _, err := (RSSProbe{Run: failing}).Measure(context.Background(), 100); err == nil {
		t.Fatal("a failing ps must fail the RSS probe")
	}
}

func TestRSSProbeFailsWhenTheRSSQueryFails(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "ps" && len(args) > 0 && args[0] == "-axo" {
			return psTree, nil
		}
		return "", errors.New("rss query failed")
	}
	if _, err := (RSSProbe{Run: run}).Measure(context.Background(), 100); err == nil {
		t.Fatal("a failing rss query must fail the RSS probe")
	}
}

func TestRSSProbeReportsNoFigureWhenNothingIsResident(t *testing.T) {
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "ps" && len(args) > 0 && args[0] == "-axo" {
			return psTree, nil
		}
		return "", nil
	}
	if _, err := (RSSProbe{Run: run}).Measure(context.Background(), 100); !errors.Is(err, ErrNoFigure) {
		t.Fatalf("empty rss output: %v", err)
	}
}

func TestNvidiaProbeQueriesExactArguments(t *testing.T) {
	var got []string
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "nvidia-smi" {
			got = args
			return "200, 1\n", nil
		}
		return psTree, nil
	}
	if _, err := (NvidiaProbe{Run: run}).Measure(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	want := []string{"--query-compute-apps=pid,used_memory", "--format=csv,noheader,nounits"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nvidia-smi args %q, want %q", got, want)
	}
}

func TestDefaultDetectsTheDriverWithListFlag(t *testing.T) {
	var got []string
	run := func(_ context.Context, name string, args ...string) (string, error) {
		if name == "nvidia-smi" {
			got = args
		}
		return "GPU 0: NVIDIA GeForce RTX 5090\n", nil
	}
	Default("linux", run)
	if want := []string{"-L"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("detection args %q, want %q", got, want)
	}
}

func TestNvidiaProbeSkipsRowsWithoutAFigure(t *testing.T) {
	run := func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "ps" {
			return psTree, nil
		}
		return "200, [N/A]\n300, 100\n", nil
	}
	m, err := NvidiaProbe{Run: run}.Measure(context.Background(), 100)
	if err != nil {
		t.Fatalf("an [N/A] row must be skipped, not fail the probe: %v", err)
	}
	if m.Bytes != 100<<20 || m.Source != "measured" {
		t.Fatalf("memory %+v", m)
	}
}

func TestNvidiaProbeRefusesANonPositiveRoot(t *testing.T) {
	calls := 0
	run := func(context.Context, string, ...string) (string, error) {
		calls++
		return "", nil
	}
	for _, root := range []int{0, -1} {
		_, err := NvidiaProbe{Run: run}.Measure(context.Background(), root)
		if err == nil || err.Error() != "measure: root pid must be positive" {
			t.Fatalf("root %d: %v", root, err)
		}
	}
	if calls != 0 {
		t.Fatalf("%d commands ran for a non-positive root", calls)
	}
}

func TestRSSProbeRefusesANonPositiveRoot(t *testing.T) {
	calls := 0
	run := func(context.Context, string, ...string) (string, error) {
		calls++
		return "", nil
	}
	for _, root := range []int{0, -1} {
		_, err := RSSProbe{Run: run}.Measure(context.Background(), root)
		if err == nil || err.Error() != "measure: root pid must be positive" {
			t.Fatalf("root %d: %v", root, err)
		}
	}
	if calls != 0 {
		t.Fatalf("%d commands ran for a non-positive root", calls)
	}
}
