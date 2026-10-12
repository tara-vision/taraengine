package catalog

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSetCapabilitiesUpdatesAnEntryTheIndexHolds(t *testing.T) {
	idx := &Index{Path: filepath.Join(t.TempDir(), "models", "index.json")}
	if err := idx.Add(IndexEntry{Name: "mine", Path: "/m/mine.gguf", Size: 1, NativeContext: 2048}); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetCapabilities("mine", []string{"tools", "thinking"}); err != nil {
		t.Fatal(err)
	}
	entries, err := idx.Load()
	if err != nil || len(entries) != 1 || !slices.Equal(entries[0].Capabilities, []string{"tools", "thinking"}) ||
		entries[0].NativeContext != 2048 {
		t.Fatalf("the list is saved and the rest of the entry kept: %+v %v", entries, err)
	}
	before, err := os.Stat(idx.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := idx.SetCapabilities("mine", []string{"tools", "thinking"}); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(idx.Path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("an unchanged list is not written again: %v", err)
	}
}

func TestSetCapabilitiesLeavesANameTheIndexDoesNotHoldAlone(t *testing.T) {
	idx := &Index{Path: filepath.Join(t.TempDir(), "models", "index.json")}
	if err := idx.SetCapabilities("gemma4:12b", []string{"tools"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(idx.Path); !os.IsNotExist(err) {
		t.Fatalf("a registry model is never written into the index: %v", err)
	}
	if err := idx.Add(IndexEntry{Name: "mine", Path: "/m/mine.gguf", Size: 1}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Remove("mine"); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetCapabilities("mine", []string{"tools"}); err != nil {
		t.Fatal(err)
	}
	if entries, err := idx.Load(); err != nil || len(entries) != 0 {
		t.Fatalf("an import removed while its props were read does not come back: %+v %v", entries, err)
	}
}

func TestSetCapabilitiesReportsAnIndexItCannotRead(t *testing.T) {
	idx := &Index{Path: filepath.Join(t.TempDir(), "index.json")}
	if err := os.WriteFile(idx.Path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := idx.SetCapabilities("mine", []string{"tools"}); err == nil {
		t.Fatal("a broken index must be reported")
	}
}

func TestItemsCarryTheirCapabilities(t *testing.T) {
	c, _ := testCatalog(t)
	gemma, _ := c.Registry.Find("gemma4:12b")
	local := filepath.Join(t.TempDir(), "mine.gguf")
	touch(t, local)
	if err := c.Index.Add(IndexEntry{Name: "mine", Path: local, Size: 1, Capabilities: []string{"tools"}}); err != nil {
		t.Fatal(err)
	}
	items, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		switch it.Name {
		case "gemma4:12b":
			if !slices.Equal(it.Capabilities, gemma.Capabilities) {
				t.Fatalf("a catalog model has the registry's capabilities: %v", it.Capabilities)
			}
		case "mine":
			if !slices.Equal(it.Capabilities, []string{"tools"}) {
				t.Fatalf("an import has the capabilities its index entry keeps: %v", it.Capabilities)
			}
		}
	}
}
