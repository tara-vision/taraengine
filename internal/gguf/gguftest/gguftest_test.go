package gguftest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestHeaderWritesTheLayoutOfTheFormat(t *testing.T) {
	got := Header(3, U32("a.context_length", 7))
	want := []byte{
		'G', 'G', 'U', 'F', 3, 0, 0, 0, // the magic and the version
		0, 0, 0, 0, 0, 0, 0, 0, // no tensors
		1, 0, 0, 0, 0, 0, 0, 0, // one pair
		16, 0, 0, 0, 0, 0, 0, 0, 'a', '.', 'c', 'o', 'n', 't', 'e', 'x', 't', '_', 'l', 'e', 'n', 'g', 't', 'h',
		4, 0, 0, 0, // a uint32
		7, 0, 0, 0,
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("header\n got %v\nwant %v", got, want)
	}
}

func TestValuesAreLittleEndian(t *testing.T) {
	rows := []struct {
		kv    KV
		typ   uint32
		value []byte
	}{
		{String("k", "ab"), TypeString, []byte{2, 0, 0, 0, 0, 0, 0, 0, 'a', 'b'}},
		{U64("k", 1), TypeUint64, []byte{1, 0, 0, 0, 0, 0, 0, 0}},
		{I32("k", -1), TypeInt32, []byte{0xff, 0xff, 0xff, 0xff}},
		{F32("k", 1), TypeFloat32, []byte{0, 0, 0x80, 0x3f}},
		{Strings("k", "a"), TypeArray, []byte{8, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 'a'}},
		{Array("k", TypeUint8, 3, []byte{9}), TypeArray, []byte{0, 0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 9}},
	}
	for _, row := range rows {
		if row.kv.Key != "k" || row.kv.Type != row.typ || !bytes.Equal(row.kv.Value, row.value) {
			t.Errorf("%+v, want type %d and value %v", row.kv, row.typ, row.value)
		}
	}
}

func TestWriteModelWritesTheModelHeader(t *testing.T) {
	path := WriteModel(t, t.TempDir(), "m.gguf", "llama", 2048)
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, Model("llama", 2048)) {
		t.Fatalf("%s: %v", path, err)
	}
	for _, key := range []string{"general.architecture", "llama.context_length", "tokenizer.ggml.tokens"} {
		if !bytes.Contains(data, []byte(key)) {
			t.Errorf("the model header lacks %s", key)
		}
	}
}

// fatalRecorder notes a Fatal instead of ending the test, so that a helper's own failure can be seen.
type fatalRecorder struct {
	testing.TB
	failed bool
}

func (r *fatalRecorder) Fatal(...any) { r.failed = true }

func TestWriteModelFailsTheTestWhenItCannotWrite(t *testing.T) {
	rec := &fatalRecorder{TB: t}
	WriteModel(rec, filepath.Join(t.TempDir(), "absent"), "m.gguf", "llama", 2048)
	if !rec.failed {
		t.Fatal("a header that cannot be written fails the test")
	}
}
