package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStateTransitionsAreCopies(t *testing.T) {
	var st State
	one := st.MarkFailedLoad("a")
	two := one.MarkFailedLoad("a")
	if st.FailedLoads["a"] != 0 || one.FailedLoads["a"] != 1 || two.FailedLoads["a"] != 2 {
		t.Fatalf("counts: %v %v %v", st.FailedLoads, one.FailedLoads, two.FailedLoads)
	}
	if one.IsFaulty("a") || !two.IsFaulty("a") || two.IsFaulty("b") {
		t.Fatal("faulty rule is two consecutive failed loads")
	}
	cleared := two.ClearFailures("a")
	if cleared.IsFaulty("a") || two.FailedLoads["a"] != 2 {
		t.Fatal("clear must not touch the original")
	}
	withCtx := cleared.WithOverride("a", 16384).WithLastLoaded("a")
	if withCtx.Overrides["a"] != 16384 || withCtx.LastLoaded != "a" || cleared.LastLoaded != "" {
		t.Fatalf("override or last loaded wrong: %+v", withCtx)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := Store{Path: filepath.Join(t.TempDir(), "state.json")}
	st, err := s.Load()
	if err != nil || st.FailedLoads == nil || st.Overrides == nil {
		t.Fatalf("empty load must give usable maps: %+v %v", st, err)
	}
	st = st.MarkFailedLoad("x").WithOverride("y", 8192).WithLastLoaded("y")
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	back, err := s.Load()
	if err != nil || back.FailedLoads["x"] != 1 || back.Overrides["y"] != 8192 || back.LastLoaded != "y" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	if err := os.WriteFile(s.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("broken state accepted")
	}
}

func TestLoadReportsReadFailure(t *testing.T) {
	s := Store{Path: t.TempDir()}
	if _, err := s.Load(); err == nil {
		t.Fatal("unreadable state accepted")
	}
}

func TestSaveReportsWriteFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string) string
	}{
		{
			name: "parent is a file",
			setup: func(t *testing.T, dir string) string {
				blocker := filepath.Join(dir, "blocker")
				if err := os.WriteFile(blocker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(blocker, "state.json")
			},
		},
		{
			name: "temporary path is a directory",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "state.json")
				if err := os.Mkdir(path+".tmp", 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "target is a directory",
			setup: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "state.json")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Store{Path: tc.setup(t, t.TempDir())}
			if err := s.Save(State{}); err == nil {
				t.Fatal("failed write reported as success")
			}
		})
	}
}

func TestSaveHidesDirectoryFromOtherUsers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s := Store{Path: filepath.Join(dir, "state.json")}
	if err := s.Save(State{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The umask can only narrow the mode, so the bits for other users must stay clear.
	if perm := info.Mode().Perm(); perm&0o007 != 0 {
		t.Fatalf("state directory mode %v is open to other users", perm)
	}
}

func TestTransitionsLeaveReceiverUntouched(t *testing.T) {
	// Transitions must not write through the receiver: a later task copies state under a lock and
	// marshals the copy outside it.
	populated := func() State {
		return State{FailedLoads: map[string]int{"a": 1}, LastLoaded: "a", Overrides: map[string]int{"a": 4096}}
	}
	transitions := []struct {
		name  string
		apply func(State) State
	}{
		{"MarkFailedLoad", func(s State) State { return s.MarkFailedLoad("a") }},
		{"ClearFailures", func(s State) State { return s.ClearFailures("a") }},
		{"WithOverride", func(s State) State { return s.WithOverride("a", 16384) }},
		{"WithLastLoaded", func(s State) State { return s.WithLastLoaded("b") }},
	}
	for _, tc := range transitions {
		t.Run(tc.name, func(t *testing.T) {
			receiver := populated()
			tc.apply(receiver)
			if want := populated(); !reflect.DeepEqual(receiver, want) {
				t.Fatalf("%s changed its receiver: got %+v, want %+v", tc.name, receiver, want)
			}
		})
	}
}

func TestLoadRefusesEmptyPath(t *testing.T) {
	var s Store
	if _, err := s.Load(); err == nil || err.Error() != "state: path is empty" {
		t.Fatalf("empty path not refused: %v", err)
	}
}

func TestSaveRefusesEmptyPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	var s Store
	if err := s.Save(State{}); err == nil || err.Error() != "state: path is empty" {
		t.Fatalf("empty path not refused: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused save left files in the working directory: %v", entries)
	}
}
