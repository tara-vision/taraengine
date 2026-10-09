package cli

import (
	"errors"
	"io/fs"
	"strings"

	"github.com/tara-vision/taraengine/internal/config"
	"github.com/tara-vision/taraengine/internal/runtime"
)

// runtimeStatus is the state of the runtime a daemon runs on: the configured runtime_dir when there is one, else the
// pinned build under the data directory. The daemon, /api/doctor and the doctor command all ask it, so that they
// cannot disagree about which runtime is meant.
func runtimeStatus(runtimeDir string, inst *runtime.Installer, lock runtime.Lock) runtime.Status {
	if runtimeDir == "" {
		return inst.Status(lock)
	}
	return configuredRuntime(runtimeDir)
}

// configuredRuntime checks the runtime_dir of the configuration. The build is the one its manifest names, when the
// manifest can be read. The installer works under the data directory and cannot repair a directory that the
// configuration names, so a problem names the directory and the config key and sends no one to the installer.
func configuredRuntime(dir string) runtime.Status {
	m, err := runtime.Verify(dir)
	st := runtime.Status{
		Installed: err == nil || m.Build != "", Verified: err == nil, Build: m.Build, Dir: dir, Binary: runtime.Binary(dir),
	}
	if err != nil {
		st.Err = "runtime_dir " + dir + " " + runtimeDirProblem(m, err) + "; check runtime_dir in the config"
	}
	return st
}

// runtimeDirProblem says what is wrong with a runtime directory that did not verify. Verify returns the manifest it
// could read, so an empty build means that there was none to read. It wraps whatever stopped it from reading the
// binary, and returns a plain error for a binary that was read and hashes to something else.
func runtimeDirProblem(m runtime.Manifest, err error) string {
	switch {
	case m.Build == "" && errors.Is(err, fs.ErrNotExist):
		return "has no manifest.json"
	case m.Build == "":
		return "has an unreadable manifest.json (" + err.Error() + ")"
	case errors.Is(err, fs.ErrNotExist):
		return "has a manifest.json but no llama-server"
	case errors.Unwrap(err) != nil:
		return "holds a llama-server that cannot be read (" + strings.TrimPrefix(err.Error(), "runtime: ") + ")"
	}
	return "holds a llama-server that does not match its manifest.json"
}

// resolveRuntime is the runtime the daemon runs from and its build: the configured directory, or the verified pinned
// build. Attach mode needs none, so a runtime that does not verify is no reason to refuse it.
func resolveRuntime(
	cfg config.Config, inst *runtime.Installer, lock runtime.Lock, attached bool,
) (string, string, error) {
	st := runtimeStatus(cfg.RuntimeDir, inst, lock)
	switch {
	case st.Verified || attached:
		return st.Dir, st.Build, nil
	case cfg.RuntimeDir != "":
		return "", "", errors.New(st.Err)
	}
	return "", "", errors.New("no verified runtime: " + strings.TrimPrefix(st.Err, "runtime: ") + installHint(st.Err))
}

// installHint is what to append to a problem with the pinned build so that it names the command that fixes it. A
// message that already names it gets nothing.
func installHint(problem string) string {
	const fix = "taraengine runtime install"
	if strings.Contains(problem, fix) {
		return ""
	}
	return "; run `" + fix + "`"
}
