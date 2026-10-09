package runtime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tarEntry struct {
	name, link, body string
	typ              byte
	mode             int64
	pax              map[string]string
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body)), PAXRecords: e.pax,
		}
		if hdr.Mode == 0 && e.typ != tar.TypeXGlobalHeader { // a global header carries PAX records and nothing else
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func runtimeTarball(t *testing.T, top string) []byte {
	return makeTarGz(t, []tarEntry{
		{name: top + "/", typ: tar.TypeDir, mode: 0o755},
		{name: top + "/llama-server", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\necho server\n"},
		{name: top + "/LICENSE", typ: tar.TypeReg, body: "MIT"},
		{name: top + "/libx.so.1", typ: tar.TypeReg, body: "lib"},
		{name: top + "/libx.so", typ: tar.TypeSymlink, link: "libx.so.1"},
	})
}

func TestUnpackStripsTheTopDirAndKeepsSiblingSymlinks(t *testing.T) {
	dest := t.TempDir()
	if err := Unpack(bytes.NewReader(runtimeTarball(t, "llama-bTEST")), dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "llama-server"))
	if err != nil || info.Mode()&0o100 == 0 {
		t.Fatalf("llama-server missing or not executable: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "LICENSE")); err != nil {
		t.Fatal("LICENSE missing")
	}
	if target, err := os.Readlink(filepath.Join(dest, "libx.so")); err != nil || target != "libx.so.1" {
		t.Fatalf("symlink target %q, %v", target, err)
	}
}

func TestUnpackRefusesEscapes(t *testing.T) {
	cases := []struct {
		name  string
		entry tarEntry
	}{
		{"dot dot", tarEntry{name: "llama-b/../../evil", typ: tar.TypeReg, body: "x"}},
		{"absolute", tarEntry{name: "/etc/evil", typ: tar.TypeReg, body: "x"}},
		{"symlink up", tarEntry{name: "llama-b/lib.so", typ: tar.TypeSymlink, link: "../../etc/passwd"}},
		{"symlink absolute", tarEntry{name: "llama-b/lib.so", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		{"symlink into subdir", tarEntry{name: "llama-b/lib.so", typ: tar.TypeSymlink, link: "sub/lib.so"}},
		{"hard link", tarEntry{name: "llama-b/lib.so", typ: tar.TypeLink, link: "llama-server"}},
		{"device", tarEntry{name: "llama-b/dev", typ: tar.TypeChar}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			err := Unpack(bytes.NewReader(makeTarGz(t, []tarEntry{{name: "llama-b/good", typ: tar.TypeReg, body: "x"}, tc.entry})), dest)
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("want a refusal, got %v", err)
			}
			entries, _ := os.ReadDir(dest)
			if len(entries) != 0 {
				t.Fatalf("dest is not empty after a refusal: %v", entries)
			}
		})
	}
}

func TestUnpackRejectsGarbage(t *testing.T) {
	if err := Unpack(bytes.NewReader([]byte("not a tarball")), t.TempDir()); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestUnpackKeepsNestedDirectories(t *testing.T) {
	dest := t.TempDir()
	tgz := makeTarGz(t, []tarEntry{
		{name: "llama-b/sub/", typ: tar.TypeDir, mode: 0o755},
		{name: "llama-b/sub/file", typ: tar.TypeReg, body: "nested"},
	})
	if err := Unpack(bytes.NewReader(tgz), dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "sub", "file")); err != nil || string(got) != "nested" {
		t.Fatalf("nested file %q, %v", got, err)
	}
}

func TestUnpackRefusesAnEntryOutsideTheTopDir(t *testing.T) {
	cases := []struct {
		name  string
		entry tarEntry
	}{
		{"file", tarEntry{name: "loose", typ: tar.TypeReg, body: "x"}},
		{"file behind dot slash", tarEntry{name: "./loose", typ: tar.TypeReg, body: "x"}},
		{"link", tarEntry{name: "loose", typ: tar.TypeSymlink, link: "llama-server"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			tgz := makeTarGz(t, []tarEntry{{name: "llama-b/llama-server", typ: tar.TypeReg, mode: 0o755, body: "server"}, tc.entry})
			err := Unpack(bytes.NewReader(tgz), dest)
			if err == nil || !strings.Contains(err.Error(), "top-level directory") || !strings.Contains(err.Error(), "loose") {
				t.Fatalf("want an error that names the loose entry and the top-level directory, got %v", err)
			}
			if entries, _ := os.ReadDir(dest); len(entries) != 0 {
				t.Fatalf("a refused archive left %v behind", entries)
			}
		})
	}
}

func TestUnpackRefusesAnArchiveWhoseLibrariesAreAtTheTopLevel(t *testing.T) {
	flat := makeTarGz(t, []tarEntry{
		{name: "libcublas.so.12", typ: tar.TypeReg, body: "a"},
		{name: "libcudart.so.12", typ: tar.TypeReg, body: "b"},
	})
	dest := t.TempDir()
	err := Unpack(bytes.NewReader(flat), dest)
	if err == nil || !strings.Contains(err.Error(), "libcublas.so.12") {
		t.Fatalf("want the first loose entry named, got %v", err)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatalf("a flat archive left %v behind", entries)
	}
}

func TestUnpackRefusesAnArchiveWithoutARegularFile(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"nothing at all", nil},
		{"only the top directory", []tarEntry{{name: "llama-b/", typ: tar.TypeDir, mode: 0o755}}},
		{"only directories", []tarEntry{
			{name: "llama-b/", typ: tar.TypeDir, mode: 0o755},
			{name: "llama-b/sub/", typ: tar.TypeDir, mode: 0o755},
		}},
		{"only a link", []tarEntry{
			{name: "llama-b/", typ: tar.TypeDir, mode: 0o755},
			{name: "llama-b/lib.so", typ: tar.TypeSymlink, link: "lib.so.1"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			err := Unpack(bytes.NewReader(makeTarGz(t, tc.entries)), dest)
			if err == nil || !strings.Contains(err.Error(), "no regular file") {
				t.Fatalf("want the missing regular file named, got %v", err)
			}
			if entries, _ := os.ReadDir(dest); len(entries) != 0 {
				t.Fatalf("an archive without files left %v behind", entries)
			}
		})
	}
}

func TestUnpackIgnoresTheRootAndAPaxGlobalHeader(t *testing.T) {
	dest := t.TempDir()
	tgz := makeTarGz(t, []tarEntry{
		{name: "pax_global_header", typ: tar.TypeXGlobalHeader, pax: map[string]string{"comment": "0123456789abcdef"}},
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "./llama-b", typ: tar.TypeDir, mode: 0o755},
		{name: "./llama-b/llama-server", typ: tar.TypeReg, mode: 0o755, body: "server"},
	})
	if err := Unpack(bytes.NewReader(tgz), dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 1 || entries[0].Name() != "llama-server" {
		t.Fatalf("want only llama-server, got %v", entries)
	}
}

func TestUnpackFailsOnEntriesItCannotPlace(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"dot entry names dest itself", []tarEntry{{name: "llama-b/.", typ: tar.TypeReg, body: "x"}}},
		{"symlink under a file", []tarEntry{
			{name: "llama-b/f", typ: tar.TypeReg, body: "x"},
			{name: "llama-b/f/link", typ: tar.TypeSymlink, link: "x"},
		}},
		{"symlink over a directory", []tarEntry{
			{name: "llama-b/sub/x", typ: tar.TypeReg, body: "x"},
			{name: "llama-b/sub", typ: tar.TypeSymlink, link: "x"},
		}},
		{"file over a directory", []tarEntry{
			{name: "llama-b/sub/x", typ: tar.TypeReg, body: "x"},
			{name: "llama-b/sub", typ: tar.TypeReg, body: "y"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Unpack(bytes.NewReader(makeTarGz(t, tc.entries)), t.TempDir()); err == nil {
				t.Fatal("entry accepted")
			}
		})
	}
}

func TestUnpackRejectsAGzipPayloadThatIsNotATar(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(bytes.Repeat([]byte("x"), 512)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(&buf, t.TempDir()); err == nil {
		t.Fatal("gzip of non-tar accepted")
	}
}

func TestUnpackFailsWhenTheDestIsAFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(dest, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(bytes.NewReader(runtimeTarball(t, "llama-bTEST")), dest); err == nil {
		t.Fatal("unpack into a file accepted")
	}
}

func TestUnpackFailsOnATruncatedEntry(t *testing.T) {
	body := make([]byte, 64<<10)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	full := makeTarGz(t, []tarEntry{
		{name: "llama-b/good", typ: tar.TypeReg, body: "x"},
		{name: "llama-b/big", typ: tar.TypeReg, body: string(body)},
	})
	dest := t.TempDir()
	if err := Unpack(bytes.NewReader(full[:len(full)/2]), dest); err == nil {
		t.Fatal("truncated entry accepted")
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatalf("truncated unpack left %v behind", entries)
	}
}

func TestUnpackLeavesNothingBehindWhenAnEntryIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
	}{
		{"hard link after a file", []tarEntry{
			{name: "llama-b/good", typ: tar.TypeReg, body: "x"},
			{name: "llama-b/lib.so", typ: tar.TypeLink, link: "good"},
		}},
		{"dot dot after a file", []tarEntry{
			{name: "llama-b/good", typ: tar.TypeReg, body: "x"},
			{name: "llama-b/../../evil", typ: tar.TypeReg, body: "x"},
		}},
		{"dot dot after a directory and its file", []tarEntry{
			{name: "llama-b/sub/", typ: tar.TypeDir, mode: 0o755},
			{name: "llama-b/sub/file", typ: tar.TypeReg, body: "x"},
			{name: "llama-b/sub/../../evil", typ: tar.TypeReg, body: "x"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			if err := Unpack(bytes.NewReader(makeTarGz(t, tc.entries)), dest); err == nil {
				t.Fatal("refused entry accepted")
			}
			if entries, _ := os.ReadDir(dest); len(entries) != 0 {
				t.Fatalf("refused unpack left %v behind", entries)
			}
		})
	}
}

func TestUnpackFailsWhenAFileNameIsTooLong(t *testing.T) {
	tooLong := strings.Repeat("b", 300)
	tgz := makeTarGz(t, []tarEntry{{name: "llama-b/" + tooLong, typ: tar.TypeReg, body: "x"}})
	if err := Unpack(bytes.NewReader(tgz), t.TempDir()); err == nil {
		t.Fatal("name over the file system limit accepted")
	}
}

func TestUnpackFailsWhenALinkTargetIsTooLong(t *testing.T) {
	tgz := makeTarGz(t, []tarEntry{{name: "llama-b/lib.so", typ: tar.TypeSymlink, link: strings.Repeat("a", 8192)}})
	if err := Unpack(bytes.NewReader(tgz), t.TempDir()); err == nil {
		t.Fatal("link target over the path limit accepted")
	}
}

func TestUnpackRefusesLinkTargetsThatAreNotBareNames(t *testing.T) {
	cases := []struct{ name, link string }{{"empty", ""}, {"dot", "."}, {"dot dot", ".."}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := t.TempDir()
			err := Unpack(bytes.NewReader(makeTarGz(t, []tarEntry{{name: "llama-b/lib.so", typ: tar.TypeSymlink, link: tc.link}})), dest)
			if err == nil || !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("want a refusal, got %v", err)
			}
			if entries, _ := os.ReadDir(dest); len(entries) != 0 {
				t.Fatalf("refused link left %v behind", entries)
			}
		})
	}
}

func TestUnpackAcceptsADotSlashPrefix(t *testing.T) {
	dest := t.TempDir()
	tgz := makeTarGz(t, []tarEntry{{name: "./llama-b/llama-server", typ: tar.TypeReg, mode: 0o755, body: "server"}})
	if err := Unpack(bytes.NewReader(tgz), dest); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dest, "llama-server")); err != nil || string(got) != "server" {
		t.Fatalf("llama-server %q, %v", got, err)
	}
}
