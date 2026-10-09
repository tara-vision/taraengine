package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// crashedSwap installs once, then puts the install where a swap that died between its two renames leaves it: in the
// aside directory, with no install directory.
func crashedSwap(t *testing.T) (inst *Installer, lock Lock, p Platform, dir, aside string) {
	t.Helper()
	inst, lock, p = darwinInstaller(t)
	dir, err := inst.Install(context.Background(), lock, p, false)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	aside = dir + ".old"
	if err := os.Rename(dir, aside); err != nil {
		t.Fatal(err)
	}
	return inst, lock, p, dir, aside
}

func TestFailedInstallPutsTheOldInstallBackFromTheAsideDirectory(t *testing.T) {
	inst, lock, p, dir, aside := crashedSwap(t)
	lock.Assets["darwin/arm64"] = Asset{Asset: "llama-bTEST-bin-absent.tar.gz", SHA256: strings.Repeat("0", 64)}
	if _, err := inst.Install(context.Background(), lock, p, false); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want the failed download, got %v", err)
	}
	if _, err := Verify(dir); err != nil {
		t.Fatalf("the only good copy was not put back: %v", err)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatal("aside directory left behind after it was put back")
	}
}

func TestInstallAfterACrashedSwapReplacesTheRestoredInstall(t *testing.T) {
	inst, lock, p, dir, aside := crashedSwap(t)
	if _, err := inst.Install(context.Background(), lock, p, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := Verify(dir); err != nil {
		t.Fatalf("install not verifiable: %v", err)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatal("aside directory left behind after a successful install")
	}
}

func TestInstallStopsWhenTheOldInstallCannotBePutBack(t *testing.T) {
	inst, lock, p, dir, aside := crashedSwap(t)
	failRename(t, func(from, _ string) bool { return from == aside }, syscall.EACCES)
	_, err := inst.Install(context.Background(), lock, p, false)
	if err == nil || !strings.Contains(err.Error(), "restoring") || !strings.Contains(err.Error(), syscall.EACCES.Error()) {
		t.Fatalf("want the failed restore named with its cause, got %v", err)
	}
	if _, err := Verify(aside); err != nil {
		t.Fatalf("the old install was not kept in the aside directory: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("an install directory appeared although the restore failed")
	}
	if _, err := os.Stat(filepath.Join(inst.Dir, "bTEST.partial")); !os.IsNotExist(err) {
		t.Fatal("staging directory left behind")
	}
}
