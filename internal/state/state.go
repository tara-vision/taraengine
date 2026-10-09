// Package state persists what the daemon must remember across restarts: consecutive failed loads
// per model (two make a model faulty), the context override per model, the last loaded model.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// State is the persisted record. Methods return modified copies; the receiver is never changed.
type State struct {
	FailedLoads map[string]int `json:"failed_loads"`
	LastLoaded  string         `json:"last_loaded"`
	Overrides   map[string]int `json:"overrides"`
}

// Store is state.json in the data directory.
type Store struct {
	Path string
}

// errEmptyPath refuses a store without a path. Load would otherwise report an empty state, and Save
// would write a temporary file into the working directory.
var errEmptyPath = errors.New("state: path is empty")

// Load reads the state; a missing file is an empty state with usable maps. An empty Path is refused.
func (s Store) Load() (State, error) {
	if s.Path == "" {
		return State{}, errEmptyPath
	}
	st := State{}
	data, err := os.ReadFile(s.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return State{}, fmt.Errorf("state: read: %w", err)
	default:
		if err := json.Unmarshal(data, &st); err != nil {
			return State{}, fmt.Errorf("state: parse: %w", err)
		}
	}
	return st.clone(), nil
}

// Save writes the state atomically: it writes Path + ".tmp", flushes it to the disk, and renames it over
// Path, so a power cut leaves the old file or the new one and never a torn one. The temporary name is
// fixed, so only one writer may run at a time for a Path; the daemon guarantees this by serializing its
// loads. An empty Path is refused.
func (s Store) Save(st State) error {
	if s.Path == "" {
		return errEmptyPath
	}
	data, _ := json.MarshalIndent(st, "", "  ") // maps of ints and a string always encode
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o750); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	tmp := s.Path + ".tmp"
	if err := writeSynced(tmp, data); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	return nil
}

// writeSynced writes data to path and flushes it to the disk before it returns.
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // state holds no secrets
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Sync(), f.Close())
}

func (st State) clone() State {
	out := State{LastLoaded: st.LastLoaded, FailedLoads: map[string]int{}, Overrides: map[string]int{}}
	for k, v := range st.FailedLoads {
		out.FailedLoads[k] = v
	}
	for k, v := range st.Overrides {
		out.Overrides[k] = v
	}
	return out
}

// MarkFailedLoad counts one more consecutive failed load for the model.
func (st State) MarkFailedLoad(name string) State {
	out := st.clone()
	out.FailedLoads[name]++
	return out
}

// ClearFailures forgets the model's failed loads (a load succeeded).
func (st State) ClearFailures(name string) State {
	out := st.clone()
	delete(out.FailedLoads, name)
	return out
}

// IsFaulty reports two or more consecutive failed loads.
func (st State) IsFaulty(name string) bool { return st.FailedLoads[name] >= 2 }

// WithOverride records the context window the model was last loaded with on request.
func (st State) WithOverride(name string, window int) State {
	out := st.clone()
	out.Overrides[name] = window
	return out
}

// WithLastLoaded records the model loaded last.
func (st State) WithLastLoaded(name string) State {
	out := st.clone()
	out.LastLoaded = name
	return out
}
