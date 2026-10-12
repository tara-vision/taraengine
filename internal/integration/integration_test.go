//go:build integration

package integration

import "testing"

// step is one part of a scenario. The parts build on each other, so a scenario ends at the first one that fails.
type step struct {
	name string
	run  func(*testing.T, *scenario)
}

func runSteps(t *testing.T, s *scenario, steps []step) {
	t.Helper()
	for _, st := range steps {
		if !t.Run(st.name, func(t *testing.T) { st.run(t, s) }) {
			return // the cleanup of the test still stops the daemon
		}
	}
}

// TestServePullLoadChat is the life of a model on the real runtime, from the install that scripts/integration.sh
// made to the stop of the daemon.
func TestServePullLoadChat(t *testing.T) {
	s := newScenario(t, requireEnvironment(t), "stories260k")
	runSteps(t, s, []step{
		{"runtime", checkRuntime},
		{"catalog", checkCatalog},
		{"pull", checkPull},
		{"autoload", checkAutoload},
		{"load", checkLoad},
		{"ps", checkPs},
		{"chat", checkChat},
		{"stream", checkChatStream},
		{"context", checkContextWindow},
		{"cli", checkCLI},
		{"unload", checkUnload},
		{"retry", checkFailedLoadThenRetry},
		{"unload-again", checkUnload},
		{"rm", checkRemove},
		{"stop", checkStopIdle},
	})
}

// TestCatalogNameWithAColon runs a model under a name like the ones of the real catalog, gemma4:12b for one. The
// router rewrites a colon in the name of a preset section, so the engine renames the section and every request and
// every model list on the way through. Only the real router can show that the two agree. The daemon is stopped with
// the model loaded.
func TestCatalogNameWithAColon(t *testing.T) {
	s := newScenario(t, requireEnvironment(t), "stories:260k")
	runSteps(t, s, []step{
		{"pull", checkPull},
		{"load", checkLoad},
		{"chat", checkChat},
		{"stop", checkStopLoaded},
	})
}

// TestImportTakesItsWindowFromTheFile pulls the test model as an import into a daemon whose default window, twice
// the 2048 tokens the model was trained for, is larger than the model's own: the engine reads the window from the
// file's header, and the router runs the import at it.
func TestImportTakesItsWindowFromTheFile(t *testing.T) {
	s := newScenarioAt(t, requireEnvironment(t), "tinyllama-catalog", 2*trainedWindow)
	runSteps(t, s, []step{
		{"import", checkImport},
		{"load", checkImportLoad},
		{"capabilities", checkImportCapabilities},
		{"stop", checkStopIdle},
	})
}
