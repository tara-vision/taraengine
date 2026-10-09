package cli

import (
	"errors"
	"testing"

	"github.com/tara-vision/taraengine/internal/backend"
)

func TestRootRegistersEveryCommand(t *testing.T) {
	root := NewRootCommand()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, name := range []string{"serve", "pull", "list", "ps", "load", "unload", "rm", "doctor", "runtime", "version"} {
		if !have[name] {
			t.Errorf("command %q is not registered", name)
		}
	}
	runtimeCmd, _, err := root.Find([]string{"runtime"})
	if err != nil {
		t.Fatal(err)
	}
	sub := map[string]bool{}
	for _, c := range runtimeCmd.Commands() {
		sub[c.Name()] = true
	}
	for _, name := range []string{"install", "update", "status"} {
		if !sub[name] {
			t.Errorf("runtime %s is not registered", name)
		}
	}
}

func TestLoadHasItsFlags(t *testing.T) {
	root := NewRootCommand()
	load, _, err := root.Find([]string{"load"})
	if err != nil {
		t.Fatal(err)
	}
	if f := load.Flags().Lookup("context"); f == nil || f.DefValue != "0" {
		t.Errorf("load --context: %v", f)
	}
	if f := load.Flags().Lookup("force"); f == nil || f.DefValue != "false" {
		t.Errorf("load --force: %v", f)
	}
}

func TestCommandsReturnAWriteFailureOfTheirOutput(t *testing.T) {
	boom := errors.New("disk full")
	for _, c := range []struct {
		name string
		args []string
	}{
		{"version", []string{"version"}},
		{"list", []string{"list"}},
		{"ps", []string{"ps"}},
		{"load", []string{"load", "gemma4:12b"}},
		{"unload", []string{"unload", "gemma4:12b"}},
		{"rm", []string{"rm", "gemma4:12b"}},
		{"pull", []string{"pull", "gemma4:12b"}},
		{"doctor", []string{"doctor", "--data-dir", t.TempDir()}},
		{"runtime status", []string{"runtime", "status", "--data-dir", t.TempDir()}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := startDaemon(t, nil)
			d.install(t, "gemma4:12b")
			d.fake.SetModels(backend.ModelStatus{Name: "gemma4:12b", Status: backend.StatusPulled})
			root := NewRootCommand()
			w := &failingWriter{err: boom}
			root.SetOut(w)
			root.SetErr(w)
			root.SetArgs(append(c.args, "--host="+d.srv.URL))
			if err := root.Execute(); !errors.Is(err, boom) {
				t.Fatalf("got %v, want the write error", err)
			}
		})
	}
}
