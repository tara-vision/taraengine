package catalog

import (
	"errors"
	"os"

	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
)

// Item is one model the engine knows: a registry entry, pulled or not, or an index entry.
type Item struct {
	Name       string
	Entry      *registry.Entry // nil for an escape-hatch model
	Pulled     bool
	ModelPath  string
	MMProjPath string
	DraftPath  string
	Local      bool
	Inferred   bool
	Size       int64
	// Problem says why the file of an imported model is not taken for a present one although it is not simply
	// gone: the catalog could not examine it (a permission, a path below a file), it is not a regular file, or the
	// daemon cannot read it. Only the catalog sets it. Such a model is not pulled, and stays in the list, so that it
	// can still be seen and removed; a caller with a logger may say what the problem is.
	Problem string
}

// Catalog is the registry joined with the store and the index.
type Catalog struct {
	Registry *registry.Registry
	Store    pull.Store
	Index    *Index
}

// List returns every registry entry in catalog order, then the index entries by name. It fails only when the
// index itself cannot be read. A model whose file cannot be examined stays in the list as not pulled, with the
// Problem recorded, and every other item is as it would be without it.
func (c Catalog) List() ([]Item, error) {
	items := make([]Item, 0, len(c.Registry.Entries))
	for i := range c.Registry.Entries {
		items = append(items, c.fromEntry(&c.Registry.Entries[i]))
	}
	extra, err := c.Index.Load()
	if err != nil {
		return nil, err
	}
	for _, e := range extra {
		items = append(items, fromIndex(e))
	}
	return items, nil
}

// Find returns the item with that name.
func (c Catalog) Find(name string) (Item, bool, error) {
	items, err := c.List()
	if err != nil {
		return Item{}, false, err
	}
	for _, it := range items {
		if it.Name == name {
			return it, true, nil
		}
	}
	return Item{}, false, nil
}

func (c Catalog) fromEntry(e *registry.Entry) Item {
	it := Item{Name: e.Name, Entry: e, Size: e.Size, ModelPath: c.Store.Path(e.HFRepo, e.File)}
	it.Pulled = c.Store.Exists(e.HFRepo, e.File)
	if e.MMProj != nil && c.Store.Exists(e.HFRepo, e.MMProj.File) {
		it.MMProjPath = c.Store.Path(e.HFRepo, e.MMProj.File)
	}
	if e.MTP != nil && c.Store.Exists(e.HFRepo, e.MTP.File) {
		it.DraftPath = c.Store.Path(e.HFRepo, e.MTP.File)
	}
	return it
}

// fromIndex makes the item of an escape-hatch model. A file that is gone, a local file that was moved, leaves
// the model not pulled and says nothing more. A file that cannot be examined, is not a regular file or cannot be
// opened by this user leaves it not pulled too, with the Problem set: something is wrong with the store, and a
// quiet "not pulled" would hide it, while failing the whole list would stop every other model for one file.
func fromIndex(e IndexEntry) Item {
	it := Item{Name: e.Name, ModelPath: e.Path, Local: e.Repo == "", Inferred: true, Size: e.Size}
	info, err := os.Stat(e.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// gone; not pulled is all there is to say
	case err != nil:
		it.Problem = err.Error()
	case !info.Mode().IsRegular():
		it.Problem = e.Path + " is not a regular file"
	default:
		it.Pulled, it.Problem = readable(e.Path)
	}
	return it
}

// readable reports whether the file can be opened for reading, or why it cannot.
func readable(path string) (bool, string) {
	f, err := os.Open(path) //nolint:gosec // the path is the one the index recorded for a model of this engine
	if err != nil {
		return false, err.Error()
	}
	_ = f.Close()
	return true, ""
}
