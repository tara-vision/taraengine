// Package catalog joins the registry with what the store holds and with the escape-hatch index.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// IndexEntry is a model that came in through the escape hatch: an hf:// pull (Repo and File set)
// or a local file (Repo empty, Path the link under models/local).
type IndexEntry struct {
	Name   string `json:"name"`
	Repo   string `json:"repo,omitempty"`
	File   string `json:"file,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
}

// Index is models/index.json. Add and Remove hold mu across load and save, so writers that share
// this Index value in one process never lose each other's entries. Load takes no lock because save
// replaces the file in one rename.
type Index struct {
	Path string
	mu   sync.Mutex
}

// Load returns the entries sorted by name; a missing file is an empty index.
func (i *Index) Load() ([]IndexEntry, error) {
	data, err := os.ReadFile(i.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("catalog: read index: %w", err)
	}
	var entries []IndexEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("catalog: parse index: %w", err)
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name < entries[b].Name })
	return entries, nil
}

// Add writes e, replacing an entry of the same name.
func (i *Index) Add(e IndexEntry) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	entries, err := i.Load()
	if err != nil {
		return err
	}
	kept := entries[:0:0]
	for _, x := range entries {
		if x.Name != e.Name {
			kept = append(kept, x)
		}
	}
	return i.save(append(kept, e))
}

// Remove drops the entry with that name; an absent name is not an error.
func (i *Index) Remove(name string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	entries, err := i.Load()
	if err != nil {
		return err
	}
	kept := entries[:0:0]
	for _, x := range entries {
		if x.Name != name {
			kept = append(kept, x)
		}
	}
	return i.save(kept)
}

// save replaces the file with one rename, so a concurrent Load sees the old index or the new one. The
// temp file comes from os.CreateTemp with mode 0600 and the index keeps it on purpose: the index is
// private to the daemon, which is its only writer and reader.
func (i *Index) save(entries []IndexEntry) error {
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name < entries[b].Name })
	data, _ := json.MarshalIndent(entries, "", "  ") // cannot fail: an entry holds strings and an integer
	dir := filepath.Dir(i.Path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "index-*.json.tmp")
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	if err := publish(tmp, data, i.Path); err != nil {
		_ = os.Remove(tmp.Name()) // the remove error is ignored; the failure above is the one to report
		return fmt.Errorf("catalog: %w", err)
	}
	return nil
}

// tempFile is the part of *os.File that publish uses, so that a test can watch the order of the calls.
type tempFile interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

// publish writes data into tmp, syncs it to disk, closes it and renames it over path. The sync comes before
// the rename: a power cut after the rename must leave the old index or the whole new one, never a file whose
// data had not reached the disk yet.
func publish(tmp tempFile, data []byte, path string) error {
	_, werr := tmp.Write(data)
	if err := errors.Join(werr, tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
