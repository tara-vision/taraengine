package llamaserver

import (
	"os"
	"strings"
)

// keptVariables are the variables of the daemon's environment that the child keeps, by exact name: the
// search path, the home and temporary directories, and the locale.
var keptVariables = map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true}

// keptPrefixes keep the locale categories and the CUDA settings, such as CUDA_VISIBLE_DEVICES.
var keptPrefixes = []string{"LC_", "CUDA_"}

// childEnvironment is the child's whole environment. Of the daemon's own it keeps only what llama-server
// needs to run; the runtime's libraries go in front of both library search paths, so a path the operator
// set for CUDA is not lost; and LLAMA_CACHE names an empty directory of the engine's, so the router never
// serves a model from a cache outside the catalog. Nothing else passes: no key, no token, no LLAMA_ARG_*
// option that would change how the router runs.
func childEnvironment(environ []string, runtimeDir, cacheDir string) []string {
	out := make([]string, 0, len(environ))
	library := map[string]string{}
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		switch {
		case name == "LD_LIBRARY_PATH" || name == "DYLD_LIBRARY_PATH":
			library[name] = value
		case kept(name):
			out = append(out, kv)
		}
	}
	return append(out,
		libraryPath("LD_LIBRARY_PATH", runtimeDir, library["LD_LIBRARY_PATH"]),
		libraryPath("DYLD_LIBRARY_PATH", runtimeDir, library["DYLD_LIBRARY_PATH"]),
		"LLAMA_CACHE="+cacheDir)
}

func kept(name string) bool {
	if keptVariables[name] {
		return true
	}
	for _, prefix := range keptPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// libraryPath is one library search path variable with dir in front of the daemon's own value.
func libraryPath(name, dir, old string) string {
	if old != "" {
		return name + "=" + dir + string(os.PathListSeparator) + old
	}
	return name + "=" + dir
}
