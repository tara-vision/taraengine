package llamaserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tara-vision/taraengine/internal/backend"
	"github.com/tara-vision/taraengine/internal/state"
)

const (
	// logTailLines and logTailBytes bound the end of the child's log that a failed model carries.
	logTailLines = 40
	logTailBytes = 64 << 10
	// defaultUnloadWait bounds the wait for the router to stop a model it was told to unload: it answers
	// at once and lists the model as running for about 80 ms more. unloadPoll is the pause between reads.
	defaultUnloadWait = 2 * time.Second
	unloadPoll        = 20 * time.Millisecond
	// recordOnReadTimeout bounds the recording that a read of the model list does, every model of the read
	// included, so that a read never holds the operations lock longer.
	recordOnReadTimeout = 10 * time.Second
)

// errBackendStarted is what Start returns while a child is already supervised.
var errBackendStarted = errors.New("llamaserver: the backend is already started")

// childProcess is the supervised llama-server as the backend uses it; *Supervisor implements it.
type childProcess interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	PID() int
	Running() bool
	Unhealthy() bool
}

var _ childProcess = (*Supervisor)(nil)

// roster is what one render of the preset file declared: the models in the order they are rendered, the
// models by catalog name, and the catalog name behind each router id. The router rewrites some section
// names, so ids are only ever mapped back through this table. A roster is replaced as a whole on every
// refresh and never edited, so a reader that holds one needs no lock.
type roster struct {
	models []PresetModel
	byName map[string]PresetModel
	nameOf map[string]string // router section id -> catalog name
}

// Backend runs llama-server in router mode.
type Backend struct {
	opts       Options
	client     *Client
	target     *url.URL
	newProcess func(SupervisorOptions) childProcess // a seam: the tests replace it
	maxBody    int64                                // the largest /v1 request body the proxy buffers
	unloadWait time.Duration                        // how long an unload waits for the router to stop the model
	recordWait time.Duration                        // how long a read may record the figures it found missing

	// opMu serializes Start, Refresh, Load and Unload. Load holds it from the first status read to the
	// last save, so the state file is only ever written by one operation at a time.
	opMu sync.Mutex

	mu       sync.Mutex // guards the fields below
	lock     *os.File   // the data directory's lock while the backend is started
	heldLock *os.File   // a lock the caller took, adopted by the first Start
	sup      childProcess
	roster   roster
	memory   map[string]backend.Memory
	props    map[string]backend.Props
	noMemory map[string]bool // the running models whose run has no memory figure to give (see measureMemory)
	st       state.State
}

var _ backend.Backend = (*Backend)(nil)

// New validates the options and fills the defaults; nothing runs until Start.
func New(opts Options) (*Backend, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	target, err := opts.routerURL()
	if err != nil {
		return nil, err
	}
	return &Backend{
		opts:       opts,
		client:     NewClient(target.String(), opts.APIKey),
		target:     target,
		newProcess: func(o SupervisorOptions) childProcess { return NewSupervisor(o) },
		maxBody:    maxRequestBody,
		unloadWait: defaultUnloadWait,
		recordWait: recordOnReadTimeout,
		memory:     map[string]backend.Memory{},
		props:      map[string]backend.Props{},
		noMemory:   map[string]bool{},
		heldLock:   opts.HeldLock,
	}, nil
}

// Start takes the data directory's lock, loads the state, writes the presets, starts the child unless a
// router is attached, and syncs the router's list. While another daemon holds the data directory, Start
// fails before it reads or writes anything there and before any router is touched. A second Start while
// a child is supervised is refused. A start that fails leaves no child and no lock behind, and the
// backend can be started again.
func (b *Backend) Start(ctx context.Context) (err error) {
	b.opMu.Lock()
	defer b.opMu.Unlock()
	if b.opts.Attach == "" && b.supervisor() != nil {
		return errBackendStarted
	}
	if err := os.MkdirAll(b.opts.DataDir, 0o750); err != nil {
		return fmt.Errorf("llamaserver: %w", err)
	}
	took, err := b.holdDataDir()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil && took {
			b.releaseDataDir()
		}
	}()
	st, err := b.opts.State.Load()
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.st = st
	b.mu.Unlock()
	if err := b.writePresets(ctx); err != nil {
		return err
	}
	if b.opts.Attach == "" {
		if err := b.startChild(ctx); err != nil {
			return err
		}
	}
	if _, err := b.client.Models(ctx, true); err != nil {
		_ = b.Stop(context.WithoutCancel(ctx))
		return err
	}
	return nil
}

func (b *Backend) startChild(ctx context.Context) error {
	binary := filepath.Join(b.opts.RuntimeDir, "llama-server")
	if _, err := os.Stat(binary); err != nil {
		return fmt.Errorf("llamaserver: runtime binary: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(LogPath(b.opts.DataDir)), 0o750); err != nil {
		return fmt.Errorf("llamaserver: %w", err)
	}
	if err := os.MkdirAll(CachePath(b.opts.DataDir), 0o750); err != nil {
		return fmt.Errorf("llamaserver: %w", err)
	}
	if err := writeKeyFile(KeyPath(b.opts.DataDir), b.opts.APIKey); err != nil {
		return err
	}
	sup := b.newProcess(SupervisorOptions{
		Binary:  binary,
		Args:    b.childArgs(),
		Env:     childEnvironment(os.Environ(), b.opts.RuntimeDir, CachePath(b.opts.DataDir)),
		LogPath: LogPath(b.opts.DataDir),
		PIDFile: PIDPath(b.opts.DataDir),
		Logger:  b.opts.Logger,
		Health:  b.client.Health,
	})
	b.mu.Lock()
	b.sup = sup
	b.mu.Unlock()
	if err := sup.Start(ctx); err != nil {
		b.forget(sup)
		return err
	}
	return nil
}

// forget drops sup as the backend's child, unless a Stop has already done so.
func (b *Backend) forget(sup childProcess) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sup == sup {
		b.sup = nil
	}
}

func (b *Backend) childArgs() []string {
	o := b.opts
	return []string{
		"--models-preset", PresetPath(o.DataDir),
		"--host", "127.0.0.1", "--port", strconv.Itoa(o.Port),
		"--api-key-file", KeyPath(o.DataDir),
		"--models-max", strconv.Itoa(o.ModelsMax),
		"--sleep-idle-seconds", strconv.Itoa(o.SleepIdleSeconds),
		"--no-webui", "--log-verbosity", "2",
	}
}

// writeKeyFile stores the child's key where only this user can read it. The key goes into a private
// temporary file in the same directory, which is then renamed over the key file: the key is never in a
// looser file, and a link left at the key path is replaced, not followed.
func writeKeyFile(path, key string) error {
	if err := replaceWithPrivateFile(path, []byte(key+"\n")); err != nil {
		return fmt.Errorf("llamaserver: write the child key: %w", err)
	}
	return nil
}

// replaceWithPrivateFile puts data at path through a private temporary file and a rename.
func replaceWithPrivateFile(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*") // created with mode 0600
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	_, err = tmp.Write(data)
	err = errors.Join(err, tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	return err
}

// Stop ends the child and lets go of it, so a later Start builds a new one, and frees the data directory;
// an attached router is left alone.
func (b *Backend) Stop(ctx context.Context) error {
	b.mu.Lock()
	sup := b.sup
	b.sup = nil
	b.mu.Unlock()
	var err error
	if sup != nil {
		err = sup.Stop(ctx)
	}
	b.releaseDataDir()
	return err
}

// holdDataDir takes the data directory's lock unless the backend holds it already, and reports whether it
// took it now. A lock the caller handed over is adopted once; after that the backend takes its own.
func (b *Backend) holdDataDir() (took bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lock != nil {
		return false, nil
	}
	if b.heldLock != nil {
		b.lock, b.heldLock = b.heldLock, nil
		return true, nil
	}
	lock, err := lockDataDir(b.opts.DataDir)
	if err != nil {
		return false, err
	}
	b.lock = lock
	return true, nil
}

// releaseDataDir lets go of the data directory's lock, if the backend holds it.
func (b *Backend) releaseDataDir() {
	b.mu.Lock()
	lock := b.lock
	b.lock = nil
	b.mu.Unlock()
	if lock != nil {
		_ = lock.Close() // closing the file releases the lock
	}
}

func (b *Backend) supervisor() childProcess {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sup
}

// Refresh re-renders the preset file from the catalog and makes the router re-read it.
func (b *Backend) Refresh(ctx context.Context) error {
	b.opMu.Lock()
	defer b.opMu.Unlock()
	return b.refresh(ctx)
}

func (b *Backend) refresh(ctx context.Context) error {
	if err := b.writePresets(ctx); err != nil {
		return err
	}
	_, err := b.client.Models(ctx, true)
	return err
}

// writePresets renders the catalog into the preset file. Everything is checked before the file is
// touched, so a refused catalog leaves both the file and the roster as they were.
func (b *Backend) writePresets(ctx context.Context) error {
	b.mu.Lock()
	overrides := maps.Clone(b.st.Overrides)
	b.mu.Unlock()
	models, err := b.opts.Models(ctx, overrides)
	if err != nil {
		return fmt.Errorf("llamaserver: models: %w", err)
	}
	next, err := newRoster(models, func(kept, dropped string) {
		b.opts.Logger.Warn("model left out: another name has its router id", "model", dropped, "kept", kept,
			"id", SectionID(dropped))
	})
	if err != nil {
		return err
	}
	if err := validatePresets(b.opts.Global, next.models); err != nil {
		return err
	}
	if err := WritePresets(PresetPath(b.opts.DataDir), RenderPresets(b.opts.Global, next.models)); err != nil {
		return err
	}
	b.mu.Lock()
	b.roster = next
	b.mu.Unlock()
	return nil
}

// newRoster indexes the models by name and by router id. A name that cannot be a section header is
// refused. Of two names that share one router id the one listed first is kept and the other is left out
// and reported, since the router would serve one model under both names: the catalog lists its registry
// entries before the escape-hatch models, so a registry name always wins, and an index that holds such a
// pair can never stop the engine from starting.
func newRoster(models []PresetModel, leftOut func(kept, dropped string)) (roster, error) {
	r := roster{byName: make(map[string]PresetModel, len(models)), nameOf: make(map[string]string, len(models))}
	for _, m := range models {
		if m.Name == "" || m.Name == "*" || strings.ContainsAny(m.Name, "[]\r\n\x00") {
			return roster{}, fmt.Errorf("llamaserver: model name %q cannot be a preset section name", m.Name)
		}
		id := SectionID(m.Name)
		if other, taken := r.nameOf[id]; taken {
			leftOut(other, m.Name)
			continue
		}
		r.models = append(r.models, m)
		r.byName[m.Name] = m
		r.nameOf[id] = m.Name
	}
	return r, nil
}

// validatePresets refuses what the ini format cannot carry. The format has no escaping, so a line break
// in a value would start a new key and a key that looks like a header or a comment would change the file.
func validatePresets(global map[string]string, models []PresetModel) error {
	if err := checkEntries("the global preset", global); err != nil {
		return err
	}
	for _, m := range models {
		owner := fmt.Sprintf("model %q", m.Name)
		if err := checkEntries(owner, m.Extra); err != nil {
			return err
		}
		engine := map[string]string{"model": m.ModelPath, "mmproj": m.MMProjPath, "model-draft": m.DraftPath}
		if err := checkEntries(owner, engine); err != nil {
			return err
		}
	}
	return nil
}

// checkEntries checks the keys in sorted order, so the same input always reports the same first problem.
// Values are never echoed: they may hold anything.
func checkEntries(owner string, entries map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(entries)) {
		if reason := keyProblem(key); reason != "" {
			return fmt.Errorf("llamaserver: %s: preset key %q %s", owner, key, reason)
		}
		if strings.ContainsAny(entries[key], "\r\n\x00") {
			return fmt.Errorf("llamaserver: %s: preset key %q has a value with a line break or NUL", owner, key)
		}
	}
	return nil
}

func keyProblem(key string) string {
	switch {
	case key == "":
		return "is empty"
	case key != strings.TrimSpace(key):
		return "has leading or trailing whitespace"
	case strings.ContainsAny(key, "\r\n\x00"):
		return "contains a line break or NUL"
	case strings.Contains(key, "="):
		return "contains ="
	case strings.ContainsAny(key[:1], "[#;"):
		return "starts with [, # or ;"
	}
	return ""
}

// Models lists the catalog models the router knows, with the status rules of the spec. A model the router lists on
// its own, such as one from its cache, is not ours and does not appear. A failed or faulty model carries the end of
// the child's log. A running model found without its figures, such as one a request brought up on its own, gets
// them recorded here when no operation holds the backend, and the answer shows them.
func (b *Backend) Models(ctx context.Context) ([]backend.ModelStatus, error) {
	list, err := b.client.Models(ctx, false)
	if err != nil {
		return nil, err
	}
	out, due := b.observe(list)
	if len(due) > 0 {
		out = b.recordDue(ctx, out, due)
	}
	attachLogTail(out, LogPath(b.opts.DataDir))
	return out, nil
}

// observe turns the router's list into statuses while holding the lock, and names the running models whose
// figures are due.
func (b *Backend) observe(list []RouterModel) ([]backend.ModelStatus, []dueModel) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]backend.ModelStatus, 0, len(list))
	var due []dueModel
	for _, m := range list {
		name, ours := b.roster.nameOf[m.ID]
		if !ours {
			continue
		}
		ms := b.observeLocked(name, m)
		out = append(out, ms)
		if ms.Status != backend.StatusLoaded && ms.Status != backend.StatusSleeping {
			continue
		}
		asleep := ms.Status == backend.StatusSleeping
		if need := b.missingLocked(name, asleep); need.props || need.memory {
			due = append(due, dueModel{name: name, asleep: asleep})
		}
	}
	return out, due
}

// observeLocked reads one router entry. A model the router is running, or is loading again, as it does
// for one woken from its sleep, shows the figures cached when it loaded; a model the router has stopped
// has its figures forgotten, so one that comes back on its own, as an autoload does, never shows the
// figures of an earlier run.
func (b *Backend) observeLocked(name string, m RouterModel) backend.ModelStatus {
	ms := backend.ModelStatus{Name: name, Context: b.roster.byName[name].Context, ExitCode: m.ExitCode, Failed: m.Failed}
	switch m.Status {
	case routerLoading:
		ms.Status = backend.StatusLoading
	case routerLoaded:
		ms.Status = backend.StatusLoaded
	case routerSleeping:
		ms.Status = backend.StatusSleeping
	default:
		ms.Status = b.restingStatusLocked(name, m)
	}
	if ms.Status != backend.StatusLoaded && ms.Status != backend.StatusSleeping && ms.Status != backend.StatusLoading {
		delete(b.memory, name)
		delete(b.props, name)
		delete(b.noMemory, name)
		return ms
	}
	if mem, ok := b.memory[name]; ok {
		ms.Memory = &mem
	}
	if p, ok := b.props[name]; ok {
		ms.Props = &p
	}
	return ms
}

// attachLogTail gives every failed or faulty model the end of the child's log. The file is read once, and
// only here, outside the lock. A log that is missing or unreadable leaves the tails empty.
func attachLogTail(models []backend.ModelStatus, logPath string) {
	var tail string
	read := false
	for i := range models {
		if s := models[i].Status; s != backend.StatusFailed && s != backend.StatusFaulty {
			continue
		}
		if !read {
			tail, read = readLogTail(logPath, logTailBytes, logTailLines), true
		}
		models[i].LogTail = tail
	}
}

// readLogTail is the last maxLines lines of the file, taken from its last maxBytes bytes. A file that is
// missing or cannot be read gives an empty tail: the tail is a courtesy and never a reason to fail.
func readLogTail(path string, maxBytes int64, maxLines int) string {
	info, err := os.Stat(path)
	var f *os.File
	if err == nil {
		f, err = os.Open(path) //nolint:gosec // the engine's own log
	}
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	start := max(info.Size()-maxBytes, 0)
	window := make([]byte, info.Size()-start)
	n, err := f.ReadAt(window, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	text := printable(string(window[:n]))
	if i := strings.IndexByte(text, '\n'); start > 0 && i >= 0 {
		text = text[i+1:] // the window opens inside a line, so the first piece is left out
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return strings.Join(lines[max(len(lines)-maxLines, 0):], "\n")
}

// printable is the log's text without the control characters a terminal acts on, line feeds and tabs kept: the log
// echoes an imported model's metadata, and every client that prints the tail writes it to a terminal. A byte that is
// no character becomes U+FFFD.
func printable(text string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, text)
}

// restingStatusLocked is the status of a model the router is not running.
func (b *Backend) restingStatusLocked(name string, m RouterModel) backend.Status {
	switch {
	case b.st.IsFaulty(name):
		return backend.StatusFaulty
	case m.Failed:
		return backend.StatusFailed
	default:
		return backend.StatusPulled
	}
}

// Health is the backend's own state: the child's, then whether the router answers. The router answers
// /health without a key, so this says nothing about the key.
func (b *Backend) Health(ctx context.Context) (backend.Health, error) {
	h := backend.Health{Backend: "llama.cpp", Build: b.opts.Build}
	if sup := b.supervisor(); sup != nil {
		switch {
		case sup.Unhealthy():
			h.Detail = "llama-server exited five times in ten minutes; restart taraengine"
			return h, nil
		case !sup.Running():
			h.Detail = "llama-server is not running"
			return h, nil
		}
	}
	if err := b.client.Health(ctx); err != nil {
		b.opts.Logger.Debug("router health probe failed", "error", err)
		h.Detail = "llama-server does not answer" // the health route is open, so no address and no key here
		return h, nil
	}
	h.OK = true
	return h, nil
}

// Unload asks the router to drop the model, forgets its memory and props, and returns once the router no
// longer runs it. A model that is not running is already unloaded.
func (b *Backend) Unload(ctx context.Context, name string) error {
	b.opMu.Lock()
	defer b.opMu.Unlock()
	if _, _, known := b.lookup(name); !known {
		return fmt.Errorf("%w: %s", backend.ErrUnknownModel, name)
	}
	return b.unload(ctx, name)
}

func (b *Backend) unload(ctx context.Context, name string) error {
	if err := b.client.Unload(ctx, SectionID(name)); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.memory, name)
	delete(b.props, name)
	delete(b.noMemory, name)
	b.mu.Unlock()
	return b.awaitStopped(ctx, name)
}

// awaitStopped reads the router's list until the model is neither loaded nor sleeping, for at most the
// unload wait. A load sent before that would find the old instance still up and be answered at once. The
// pause between two reads is short, and the read after it notices a caller that gave up.
func (b *Backend) awaitStopped(ctx context.Context, name string) error {
	deadline := time.Now().Add(b.unloadWait)
	for {
		m, err := b.routerStatus(ctx, name)
		if err != nil {
			return err
		}
		if m.Status != routerLoaded && m.Status != routerSleeping {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("llamaserver: the router still runs %s %s after it was told to unload it", name, b.unloadWait)
		}
		time.Sleep(unloadPoll)
	}
}

// lookup is the preset of a catalog model, and whether the state marks it faulty.
func (b *Backend) lookup(name string) (preset PresetModel, faulty, known bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	preset, known = b.roster.byName[name]
	return preset, b.st.IsFaulty(name), known
}

// updateState applies one transition to the current state and saves the result. The transition is
// always taken from the state as it is now, never from an earlier copy, so no step undoes another.
// The state in memory moves first: a save that fails does not undo a faulty mark in this process.
// Callers hold opMu, so saves never overlap.
func (b *Backend) updateState(transition func(state.State) state.State) error {
	b.mu.Lock()
	b.st = transition(b.st)
	next := b.st
	b.mu.Unlock()
	return b.opts.State.Save(next)
}

// commitState is updateState for a change that must not outlive a failed save: the state in memory
// follows the file, so a window that could not be saved is never rendered into the presets.
func (b *Backend) commitState(transition func(state.State) state.State) error {
	b.mu.Lock()
	next := transition(b.st)
	b.mu.Unlock()
	if err := b.opts.State.Save(next); err != nil {
		return err
	}
	b.mu.Lock()
	b.st = next
	b.mu.Unlock()
	return nil
}

func (b *Backend) currentRoster() roster {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.roster
}
