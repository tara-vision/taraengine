package catalog

import (
	"errors"
	"os"
	"reflect"
	"slices"

	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
)

// Item is one model the engine knows: a registry entry, pulled or not, or an index entry.
type Item struct {
	Name  string
	Entry *registry.Entry // nil for an escape-hatch model
	// Origin says where the model came from: OriginCatalog for an entry equal to the embedded catalog's, OriginRegistry
	// for one the data directory's registry.yaml holds and the embedded catalog does not, or holds differently, and
	// OriginImport for an escape-hatch model.
	Origin string
	// Repo, File, SHA256 and Quantization name what an import runs (an hf:// import has Repo and File, a local one
	// the link in ModelPath): the digest the pull verified or computed, the file type the header gave. A catalog
	// model's are in Entry.
	Repo         string
	File         string
	SHA256       string
	Quantization string
	Pulled       bool
	ModelPath    string
	MMProjPath   string
	DraftPath    string
	DraftKind    string // "mtp" or "dflash" when DraftPath is set
	Local        bool
	Inferred     bool
	Size         int64
	// NativeContext is the window the model was trained for, 0 when it is unknown: the registry's for a catalog
	// model, and for an import the one its file's header gave when it was pulled.
	NativeContext int
	// Capabilities is what the model can do: the registry's list for a catalog model, and for an import what it
	// showed the last time it ran.
	Capabilities []string
	// Problem says why the file of an imported model is not taken for a present one although it is not simply
	// gone: the catalog could not examine it (a permission, a path below a file), it is not a regular file, or the
	// daemon cannot read it. Only the catalog sets it. Such a model is not pulled, and stays in the list, so that it
	// can still be seen and removed; a caller with a logger may say what the problem is.
	Problem string
}

// Catalog is the registry joined with the store and the index.
type Catalog struct {
	Registry *registry.Registry
	// Embedded is the catalog built into the binary when Registry is a registry.yaml override, nil when Registry is
	// the embedded catalog itself; it tells an entry the operator copied verbatim from one they wrote.
	Embedded *registry.Registry
	Store    pull.Store
	Index    *Index
}

// The origins of a model.
const (
	OriginCatalog  = "catalog"
	OriginRegistry = "registry"
	OriginImport   = "import"
)

// originOf is OriginCatalog when no override serves, or when the embedded catalog holds an entry of the same
// name equal to e in every field the engine serves (the note aside: it changes nothing that runs), and
// OriginRegistry otherwise. Every difference reads registry, never a false catalog.
func (c Catalog) originOf(e *registry.Entry) string {
	if c.Embedded == nil {
		return OriginCatalog
	}
	for i := range c.Embedded.Entries {
		if emb := c.Embedded.Entries[i]; emb.Name == e.Name && servedEqual(emb, *e) {
			return OriginCatalog
		}
	}
	return OriginRegistry
}

// servedEqual compares two entries in every field but Notes.
func servedEqual(a, b registry.Entry) bool {
	a.Notes, b.Notes = "", ""
	return reflect.DeepEqual(a, b)
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
	it := Item{
		Name: e.Name, Entry: e, Origin: c.originOf(e), Size: e.Size, ModelPath: c.Store.Path(e.HFRepo, e.File),
		NativeContext: e.Context,
		Capabilities:  slices.Clone(e.Capabilities),
	}
	it.Pulled = c.Store.Exists(e.HFRepo, e.File)
	if p := e.MMProj; p != nil && c.Store.Exists(e.RepoOf(*p), p.File) {
		it.MMProjPath = c.Store.Path(e.RepoOf(*p), p.File)
	}
	if d, kind := e.Draft(); d != nil && c.Store.Exists(e.RepoOf(*d), d.File) {
		it.DraftPath, it.DraftKind = c.Store.Path(e.RepoOf(*d), d.File), kind
	}
	return it
}

// fromIndex makes the item of an escape-hatch model. A file that is gone, a local file that was moved, leaves
// the model not pulled and says nothing more. A file that cannot be examined, is not a regular file or cannot be
// opened by this user leaves it not pulled too, with the Problem set: something is wrong with the store, and a
// quiet "not pulled" would hide it, while failing the whole list would stop every other model for one file.
func fromIndex(e IndexEntry) Item {
	it := Item{
		Name: e.Name, Origin: OriginImport, ModelPath: e.Path, Local: e.Repo == "", Inferred: true, Size: e.Size,
		NativeContext: e.NativeContext, Repo: e.Repo, File: e.File, SHA256: e.SHA256, Quantization: e.Quantization,
		Capabilities: slices.Clone(e.Capabilities),
	}
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
