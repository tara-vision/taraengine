package api

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/tara-vision/taraengine/internal/catalog"
	"github.com/tara-vision/taraengine/internal/pull"
	"github.com/tara-vision/taraengine/internal/registry"
	"github.com/tara-vision/taraengine/pkg/api"
)

// The stages of a pull event that the server reports itself; the others come from the downloader.
const (
	stageResolving = "resolving"
	stageDone      = "done"
	stageError     = "error"
)

// huggingFace is the one host a pull talks to.
const huggingFace = "https://huggingface.co"

// Fetcher resolves and downloads artifacts; tests inject one that writes files.
type Fetcher interface {
	Resolve(ctx context.Context, repo, file string) (pull.Artifact, error)
	Fetch(ctx context.Context, art pull.Artifact, dest string, onProgress func(pull.Progress)) error
}

type hfFetcher struct {
	resolver *pull.Resolver
	baseURL  string
	token    string
}

// NewHFFetcher talks to Hugging Face with an optional token.
func NewHFFetcher(token string) Fetcher {
	return newHFFetcher(huggingFace, token)
}

// newHFFetcher is NewHFFetcher for a Hugging Face at another address, which is how the tests reach their own.
func newHFFetcher(baseURL, token string) *hfFetcher {
	resolver := pull.NewResolver(token)
	resolver.BaseURL = baseURL
	return &hfFetcher{resolver: resolver, baseURL: baseURL, token: token}
}

func (f *hfFetcher) Resolve(ctx context.Context, repo, file string) (pull.Artifact, error) {
	return f.resolver.Resolve(ctx, repo, file)
}

func (f *hfFetcher) Fetch(ctx context.Context, art pull.Artifact, dest string, onProgress func(pull.Progress)) error {
	d := pull.NewDownloader(f.token)
	d.BaseURL = f.baseURL
	d.OnProgress = onProgress
	return d.Fetch(ctx, art, dest)
}

// runPull performs one admitted pull. It emits the progress events, then exactly one outcome: done, or error.
func (s *Server) runPull(ctx context.Context, p plan, emit func(api.PullEvent)) error {
	err := s.transfer(ctx, p, emit)
	if err == nil {
		if refreshErr := s.deps.Backend.Refresh(ctx); refreshErr != nil {
			// What the backend says may name the child's address. The client is told the cause in fixed words.
			s.deps.Logger.Error("backend refresh failed after a pull", "model", p.ref.Name, "error", refreshErr)
			err = fmt.Errorf("%s is on disk, but the engine backend could not pick it up", p.ref.Name)
		}
	}
	// The name is given back before the last event, so the client's next request never meets this pull as running.
	p.release()
	if err != nil {
		s.deps.Logger.Warn("pull failed", "model", p.ref.Name, "error", err)
		emit(api.PullEvent{Stage: stageError, Error: err.Error()})
		return err
	}
	s.deps.Logger.Info("pulled", "model", p.ref.Name)
	emit(api.PullEvent{Stage: stageDone})
	return nil
}

func (s *Server) transfer(ctx context.Context, p plan, emit func(api.PullEvent)) error {
	switch p.ref.Kind {
	case pull.KindRegistry:
		return s.pullRegistry(ctx, p.entry, emit)
	case pull.KindHF:
		return s.pullHF(ctx, p.ref, emit)
	default:
		return s.pullLocal(p)
	}
}

// pullRegistry fetches every file of a catalog entry that is not in the store yet. Each file is checked against
// the sha256 the registry pins, before it is downloaded.
func (s *Server) pullRegistry(ctx context.Context, e registry.Entry, emit func(api.PullEvent)) error {
	store := s.deps.Catalog.Store
	for _, f := range e.Files() {
		repo := e.RepoOf(f)
		dest, err := store.Locate(repo, f.File)
		if err != nil {
			return err
		}
		if store.Exists(repo, f.File) {
			emit(api.PullEvent{Stage: stageDone, File: f.File, Bytes: f.Size, Total: f.Size})
			continue
		}
		emit(api.PullEvent{Stage: stageResolving, File: f.File, Total: f.Size})
		art, err := s.deps.Fetcher.Resolve(ctx, repo, f.File)
		if err != nil {
			return err
		}
		if art.SHA256 != f.SHA256 {
			return fmt.Errorf("pull: %s/%s: Hugging Face reports sha256 %s, the registry pins %s; "+
				"the artifact changed upstream", repo, f.File, art.SHA256, f.SHA256)
		}
		if err := s.fetch(ctx, art, dest, f.File, emit); err != nil {
			return err
		}
	}
	return nil
}

// pullHF fetches one file of any repository and records it in the index under its file name, with the window its
// header gives.
func (s *Server) pullHF(ctx context.Context, ref pull.Ref, emit func(api.PullEvent)) error {
	dest, err := s.deps.Catalog.Store.Locate(ref.Repo, ref.File)
	if err != nil {
		return err
	}
	emit(api.PullEvent{Stage: stageResolving, File: ref.File})
	art, err := s.deps.Fetcher.Resolve(ctx, ref.Repo, ref.File)
	if err != nil {
		return err
	}
	if err := s.fetch(ctx, art, dest, ref.File, emit); err != nil {
		return err
	}
	return s.deps.Catalog.Index.Add(catalog.IndexEntry{
		Name: ref.Name, Repo: ref.Repo, File: ref.File, SHA256: art.SHA256, Path: dest, Size: art.Size,
		NativeContext: s.nativeContext(ref.Name, dest),
	})
}

// pullLocal links a file of this machine into the store and records it in the index.
func (s *Server) pullLocal(p plan) error {
	link, err := s.deps.Catalog.Store.LinkLocal(p.ref.Name, p.ref.Path)
	if err != nil {
		return err
	}
	return s.deps.Catalog.Index.Add(catalog.IndexEntry{
		Name: p.ref.Name, Path: link, Size: p.local.Size(), NativeContext: s.nativeContext(p.ref.Name, link),
	})
}

// nativeContext is the window the header of a pulled file says its model was trained for. A file whose header
// gives none is still a model: its window stays unknown, and the log says why.
func (s *Server) nativeContext(name, path string) int {
	window, err := catalog.ReadNativeContext(path)
	if err != nil {
		s.deps.Logger.Warn("the native context window of the model is unknown", "model", name, "error", err)
	}
	return window
}

func (s *Server) fetch(ctx context.Context, art pull.Artifact, dest, file string, emit func(api.PullEvent)) error {
	return s.deps.Fetcher.Fetch(ctx, art, dest, func(p pull.Progress) {
		emit(api.PullEvent{Stage: p.Stage, File: file, Bytes: p.Bytes, Total: p.Total})
	})
}

// remove deletes a pulled item's files and, for an escape-hatch model, its index entry. Every file goes through
// the store, which refuses a path outside it. An index entry records the path it was stored at, and that path
// is vetted like any other: relative to the store root it goes to Remove, so a hand-edited index cannot point a
// delete at a file elsewhere.
func (s *Server) remove(it catalog.Item) error {
	store := s.deps.Catalog.Store
	if e := it.Entry; e != nil {
		// The files go repository by repository, the entry's own first: a drafter published apart from its model
		// lives under its own directory of the store.
		byRepo, repos := map[string][]string{}, []string{}
		for _, f := range e.Files() {
			repo := e.RepoOf(f)
			if _, seen := byRepo[repo]; !seen {
				repos = append(repos, repo)
			}
			byRepo[repo] = append(byRepo[repo], f.File)
		}
		for _, repo := range repos {
			if err := store.Remove(repo, byRepo[repo]...); err != nil {
				return err
			}
		}
		return nil
	}
	rel, _ := filepath.Rel(store.Dir, it.ModelPath) // when it fails rel is empty, and Remove refuses an empty name
	if err := store.Remove("", rel); err != nil {
		return err
	}
	return s.deps.Catalog.Index.Remove(it.Name)
}
