//go:build integration

package integration

import (
	"slices"
	"testing"
)

const (
	// importRef is the test model's file on the hub, pulled as an import under the name its file gives it.
	importRef  = "hf://ggml-org/models-moved/tinyllamas/stories260K.gguf"
	importName = "stories260K"
	// trainedWindow is the window stories260K was trained for, which its header gives.
	trainedWindow = 2048
)

// checkImport pulls the test model as an import into a daemon whose default window is larger than the model's own,
// and asserts that the engine read the window from the file: the catalog shows it as the native window, and the
// import's window is no larger.
func checkImport(t *testing.T, s *scenario) {
	if err := s.c.Pull(within(t, pullTimeout), importRef, nil); err != nil {
		t.Fatalf("pull of the import: %v", err)
	}
	if m := s.named(t, importName); m.Status != "pulled" || m.NativeContext != trainedWindow ||
		m.Context != trainedWindow || !m.CapabilitiesInferred {
		t.Fatalf("the import: %+v", m)
	}
}

// checkImportLoad loads the import and asserts that the router runs it at the window of its file. The scenario's
// catalog entry names the same file on the hub, so the pull of the import put that entry's file in place as well,
// and the list holds both names; the test looks at the import's.
func checkImportLoad(t *testing.T, s *scenario) {
	m, err := s.c.Load(within(t, loadTimeout), importName, 0, false)
	if err != nil || m.Status != "loaded" || m.Context != trainedWindow {
		t.Fatalf("load of the import: %+v %v", m, err)
	}
	listed := s.listed(t)
	at := slices.IndexFunc(listed, func(l listedModel) bool { return l.ID == importName })
	if at < 0 || listed[at].Status.Value != "loaded" || listed[at].Meta.NCtx != trainedWindow {
		t.Fatalf("/v1/models lists %+v, want %s loaded at %d tokens", listed, importName, trainedWindow)
	}
}

// checkImportCapabilities asserts that the import shows the same capabilities after an unload as while it ran.
// stories260K has no chat template, so the list is empty either way; that the engine keeps a list across an unload
// and a restart is proven against the fake router, whose template has tools and thinking.
func checkImportCapabilities(t *testing.T, s *scenario) {
	running := s.named(t, importName).Capabilities
	if err := s.c.Unload(within(t, callTimeout), importName); err != nil {
		t.Fatalf("unload of the import: %v", err)
	}
	if after := s.named(t, importName); after.Status != "pulled" || !slices.Equal(after.Capabilities, running) {
		t.Fatalf("capabilities %v while it ran, %v after the unload: %+v", running, after.Capabilities, after)
	}
}
