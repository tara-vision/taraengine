package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/tara-vision/taraengine/internal/backend/llamaserver"
	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
)

// refusal is a request turned down before any work starts, with the answer to give.
type refusal struct {
	status  int
	message string
	hint    string
}

func refuse(status int, message, hint string) *refusal {
	return &refusal{status: status, message: message, hint: hint}
}

func (rf *refusal) write(w http.ResponseWriter) {
	writeError(w, rf.status, rf.message, rf.hint)
}

// plan is an admitted pull: the parsed reference, what admission learned about it, and the release of its name.
type plan struct {
	ref     pull.Ref
	entry   registry.Entry // the catalog entry behind a catalog name
	local   os.FileInfo    // the file behind a local path
	release func()
}

// admit vets a reference against the catalog and the index and claims its name, so that a pull the server will not
// run is turned away with a status of its own before any stream starts. The index is read with the name held, and
// a pull adds to the index before it lets the name go, so that two pulls that would clash cannot both get through.
func (s *Server) admit(ref pull.Ref) (plan, *refusal) {
	p, refused := s.vet(ref)
	if refused != nil {
		return plan{}, refused
	}
	release, refused := s.claim(ref.Name, opPull)
	if refused != nil {
		return plan{}, refused
	}
	if refused := s.vetIndex(ref); refused != nil {
		release()
		return plan{}, refused
	}
	p.release = release
	return p, nil
}

// vet checks a reference against the catalog. A catalog name must exist, a name the catalog owns cannot be
// imported, no other name may share the router id of an imported one, and a local path must be a file.
func (s *Server) vet(ref pull.Ref) (plan, *refusal) {
	entry, inCatalog := s.deps.Catalog.Registry.Find(ref.Name)
	other, shared := s.catalogNameSharingID(ref.Name)
	switch {
	case ref.Kind == pull.KindRegistry && !inCatalog:
		return plan{}, refuse(http.StatusNotFound, "unknown catalog name: "+ref.Name, "taraengine list shows the catalog")
	case ref.Kind == pull.KindRegistry:
		return plan{ref: ref, entry: entry}, nil
	case inCatalog:
		return plan{}, refuse(http.StatusConflict, "the name "+ref.Name+" belongs to the catalog", "pull it by that name")
	case shared:
		return plan{}, sharedID(ref.Name, other, "")
	case ref.Kind == pull.KindLocal:
		return vetLocal(ref)
	}
	return plan{ref: ref}, nil
}

// catalogNameSharingID is the catalog name, other than name itself, that the router knows by the id of name.
func (s *Server) catalogNameSharingID(name string) (string, bool) {
	id := llamaserver.SectionID(name)
	for _, e := range s.deps.Catalog.Registry.Entries {
		if e.Name != name && llamaserver.SectionID(e.Name) == id {
			return e.Name, true
		}
	}
	return "", false
}

// sharedID refuses a name that would meet another name under one router id. The router serves every model by the id
// of its preset section, which is the name with each colon turned into a hyphen, and refuses a roster that gives
// one id to two models. note says where the other name was found, when that needs saying.
func sharedID(name, other, note string) *refusal {
	return refuse(http.StatusConflict,
		fmt.Sprintf("the name %s would share the engine id %s with %s%s", name, llamaserver.SectionID(name), other, note),
		"give the file a name that differs from it by more than a colon and a hyphen")
}

// vetIndex checks a reference against the models the index holds. Another name with the router id of this one is
// refused. So is a different file under a name the index has, because the index would let go of the first file
// and its blob would stay on disk with no name to remove it by; the same file again is the way a pull resumes.
func (s *Server) vetIndex(ref pull.Ref) *refusal {
	entries, err := s.deps.Catalog.Index.Load()
	if err != nil {
		s.deps.Logger.Error("catalog unreadable", "error", err)
		return refuse(http.StatusInternalServerError, err.Error(), "")
	}
	id := llamaserver.SectionID(ref.Name)
	for _, e := range entries {
		switch {
		case e.Name == ref.Name && ref.Kind != pull.KindRegistry && holdsAnother(e, ref):
			return refuse(http.StatusConflict, "the name "+ref.Name+" already holds "+describe(e),
				"remove it with taraengine rm "+ref.Name+", or give the file another name")
		case e.Name != ref.Name && llamaserver.SectionID(e.Name) == id:
			return sharedID(ref.Name, e.Name, "")
		}
	}
	return nil
}

// holdsAnother reports whether the index entry e is a file other than the one ref names. A local file is told
// apart by the path its link points to.
func holdsAnother(e catalog.IndexEntry, ref pull.Ref) bool {
	if ref.Kind == pull.KindHF {
		return e.Repo != ref.Repo || e.File != ref.File
	}
	if e.Repo != "" {
		return true
	}
	target, err := os.Readlink(e.Path)
	return err == nil && target != ref.Path
}

// describe says what an index entry holds.
func describe(e catalog.IndexEntry) string {
	if e.Repo != "" {
		return "hf://" + e.Repo + "/" + e.File
	}
	return "a local file"
}

func vetLocal(ref pull.Ref) (plan, *refusal) {
	info, err := readableFile(ref.Path)
	if err != nil {
		return plan{}, refuse(http.StatusNotFound, "pull: "+ref.Path+" is not a readable file",
			"give the absolute path of a .gguf file")
	}
	return plan{ref: ref, local: info}, nil
}

var errNotRegular = errors.New("not a regular file")

// readableFile returns what is at path, which must be a regular file this user can open for reading. The open is
// what finds a file the engine could not load before anything links to it. The type is checked first, because
// opening a named pipe would wait for a writer.
func readableFile(path string) (os.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := os.Open(path) //nolint:gosec // G304: a local model path is request input by design
	if err != nil {
		return nil, err
	}
	_ = f.Close() // only the open mattered
	return info, nil
}

// The operations that hold a name.
const (
	opPull   = "pull"
	opDelete = "delete"
)

// claim takes the in-flight slot of a name for op: one pull or delete of a model runs at a time. A pull is also
// refused while another pull runs under a name with the same router id. The refusal says which one holds the slot.
// The release frees it, once: a pull gives its name back before its last event, and the release that runs when the
// request ends then does nothing.
func (s *Server) claim(name, op string) (func(), *refusal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if holder, taken := s.busy[name]; taken {
		return nil, refuse(http.StatusConflict, "a "+holder+" of "+name+" is running", "try again when it has finished")
	}
	if other, shared := s.pullSharingID(name); op == opPull && shared {
		return nil, sharedID(name, other, " (a pull of it is running)")
	}
	s.busy[name] = op
	return sync.OnceFunc(func() { s.free(name) }), nil
}

// pullSharingID is the name of a pull in flight, other than name itself, that has the router id of name. The
// caller holds s.mu.
func (s *Server) pullSharingID(name string) (string, bool) {
	id := llamaserver.SectionID(name)
	for other, op := range s.busy {
		if op == opPull && other != name && llamaserver.SectionID(other) == id {
			return other, true
		}
	}
	return "", false
}

func (s *Server) free(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, name)
}
