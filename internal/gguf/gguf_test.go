package gguf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tara-vision/taraengine/internal/gguf/gguftest"
)

func TestTheValueTypesAgreeWithTheTestHelper(t *testing.T) {
	pairs := [][2]uint32{
		{typeUint8, gguftest.TypeUint8}, {typeInt8, gguftest.TypeInt8}, {typeUint16, gguftest.TypeUint16},
		{typeInt16, gguftest.TypeInt16}, {typeUint32, gguftest.TypeUint32}, {typeInt32, gguftest.TypeInt32},
		{typeFloat32, gguftest.TypeFloat32}, {typeBool, gguftest.TypeBool}, {typeString, gguftest.TypeString},
		{typeArray, gguftest.TypeArray}, {typeUint64, gguftest.TypeUint64}, {typeInt64, gguftest.TypeInt64},
		{typeFloat64, gguftest.TypeFloat64},
	}
	for i, p := range pairs {
		if p[0] != p[1] || p[0] != uint32(i) {
			t.Errorf("value type %d: the reader says %d, the helper %d", i, p[0], p[1])
		}
	}
}

// arch and window are the two pairs of a plain header.
var (
	arch   = gguftest.String("general.architecture", "llama")
	window = gguftest.U32("llama.context_length", 2048)
)

func TestReadFindsBothKeysInAnyOrder(t *testing.T) {
	rows := []struct {
		name   string
		header []byte
		want   Info
	}{
		{"version 3", gguftest.Header(3, arch, window), Info{Architecture: "llama", ContextLength: 2048}},
		{"version 2", gguftest.Header(2, arch, window), Info{Architecture: "llama", ContextLength: 2048}},
		{"the context length first", gguftest.Header(3, window, arch), Info{Architecture: "llama", ContextLength: 2048}},
		{"a uint64 context length", gguftest.Header(3, gguftest.String("general.architecture", "gemma3"),
			gguftest.U64("gemma3.context_length", 131072)), Info{Architecture: "gemma3", ContextLength: 131072}},
		{"the tokenizer's arrays first, as stories260K has them", gguftest.Header(3,
			gguftest.Strings("tokenizer.ggml.tokens", "<unk>", "<s>", "</s>"),
			gguftest.Array("tokenizer.ggml.scores", gguftest.TypeFloat32, 3, make([]byte, 12)),
			gguftest.Array("tokenizer.ggml.token_type", gguftest.TypeInt32, 3, make([]byte, 12)), arch, window),
			Info{Architecture: "llama", ContextLength: 2048}},
		{"other context lengths around the right one", gguftest.Header(3,
			gguftest.U32("clip.context_length", 77), arch, gguftest.U32("clip.vision.context_length", 1),
			gguftest.U32("llama.context_length", 8192)), Info{Architecture: "llama", ContextLength: 8192}},
		{"a key longer than any the reader wants", gguftest.Header(3,
			gguftest.String(strings.Repeat("k", 300), "skipped"), arch, window), Info{Architecture: "llama", ContextLength: 2048}},
		{"values of every fixed size skipped", gguftest.Header(3, gguftest.Array("u8", gguftest.TypeUint8, 2, []byte{1, 2}),
			gguftest.Array("i16", gguftest.TypeInt16, 1, []byte{1, 2}), gguftest.Array("f64", gguftest.TypeFloat64, 1, make([]byte, 8)),
			gguftest.KV{Key: "flag", Type: gguftest.TypeBool, Value: []byte{1}}, arch, window), Info{Architecture: "llama", ContextLength: 2048}},
		{"a value the engine would refuse is the caller's to judge", gguftest.Header(3, arch,
			gguftest.U64("llama.context_length", 1<<63)), Info{Architecture: "llama", ContextLength: 1 << 63}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got, err := read(bytes.NewReader(row.header))
			if err != nil || got != row.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, row.want)
			}
		})
	}
}

func TestReadKeepsTheTwoKeysWhenAPairAfterThemIsCut(t *testing.T) {
	header := gguftest.Header(3, arch, window, gguftest.String("general.name", "never read"))
	cut := header[:len(header)-5] // the third pair is cut short: the reader meets it looking for the file type and keeps the two
	if got, err := read(bytes.NewReader(cut)); err != nil || got != (Info{Architecture: "llama", ContextLength: 2048}) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestReadIgnoresWantedKeysOfAnotherType(t *testing.T) {
	rows := []struct {
		name   string
		header []byte
	}{
		{"a float context length", gguftest.Header(3, arch, gguftest.F32("llama.context_length", 2048))},
		{"a string context length", gguftest.Header(3, arch, gguftest.String("llama.context_length", "2048"))},
		{"a signed context length", gguftest.Header(3, arch, gguftest.I32("llama.context_length", 2048))},
		{"a numeric architecture", gguftest.Header(3, gguftest.U32("general.architecture", 1), window)},
		{"no context length", gguftest.Header(3, arch)},
		{"no architecture", gguftest.Header(3, window)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := read(bytes.NewReader(row.header)); !errors.Is(err, ErrMissing) {
				t.Fatalf("want ErrMissing, got %v", err)
			}
		})
	}
}

func TestReadRefusesWhatIsNotALittleEndianGGUF(t *testing.T) {
	v3 := gguftest.Header(3, arch, window)
	bigEndian := slices.Clone(v3)
	binary.BigEndian.PutUint32(bigEndian[4:8], 3)
	rows := []struct {
		name   string
		header []byte
	}{
		{"nothing", nil},
		{"a file shorter than the magic", []byte("GG")},
		{"another magic", append([]byte("GGML"), v3[4:]...)},
		{"version 1", gguftest.Header(1, arch, window)},
		{"version 4", gguftest.Header(4, arch, window)},
		{"a big-endian file", bigEndian},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := read(bytes.NewReader(row.header)); !errors.Is(err, ErrNotGGUF) {
				t.Fatalf("want ErrNotGGUF, got %v", err)
			}
		})
	}
}

// withPairs is a version 3 header that announces count pairs and goes on with raw, for pairs the helper cannot write.
func withPairs(count uint64, raw []byte) []byte {
	h := gguftest.Header(3)
	binary.LittleEndian.PutUint64(h[16:24], count)
	return append(h, raw...)
}

// contextLengths is a header with n context lengths of other architectures before its own.
func contextLengths(n int) []byte {
	kvs := make([]gguftest.KV, 0, n+2)
	for i := range n {
		kvs = append(kvs, gguftest.U32("arch"+strconv.Itoa(i)+".context_length", 512))
	}
	return gguftest.Header(3, append(kvs, arch, window)...)
}

func TestReadRefusesAHeaderItCannotWalk(t *testing.T) {
	plain := gguftest.Header(3, gguftest.Strings("tokenizer.ggml.tokens", "a", "b"), arch, window)
	rows := []struct {
		name   string
		header []byte
	}{
		{"a file cut inside a pair", plain[:40]},
		{"a pair count beyond the pairs", withPairs(1<<40, nil)},
		{"a key longer than the reader takes", withPairs(1, binary.LittleEndian.AppendUint64(nil, 1<<62))},
		{"a string longer than the reader takes", gguftest.Header(3,
			gguftest.KV{Key: "x", Type: gguftest.TypeString, Value: binary.LittleEndian.AppendUint64(nil, MaxHeaderBytes)}, arch, window)},
		{"an array longer than the reader takes", gguftest.Header(3, gguftest.Array("x", gguftest.TypeUint64, 1<<60, nil), arch, window)},
		{"a count of strings past the end of the file", gguftest.Header(3, gguftest.Array("x", gguftest.TypeString, 1<<62, nil), arch, window)},
		{"an array of arrays", gguftest.Header(3, gguftest.Array("x", gguftest.TypeArray, 1, nil), arch, window)},
		{"an array of a type the format does not have", gguftest.Header(3, gguftest.Array("x", 13, 1, nil), arch, window)},
		{"a value of a type the format does not have", gguftest.Header(3, gguftest.KV{Key: "x", Type: 13}, arch, window)},
		{"an empty architecture name", gguftest.Header(3, gguftest.String("general.architecture", ""), window)},
		{"an architecture name longer than any", gguftest.Header(3,
			gguftest.String("general.architecture", strings.Repeat("a", maxArchBytes+1)), window)},
		{"more context lengths before the architecture than the reader keeps", contextLengths(maxPending + 1)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if _, err := read(bytes.NewReader(row.header)); !errors.Is(err, ErrHeader) {
				t.Fatalf("want ErrHeader, got %v", err)
			}
		})
	}
	if got, err := read(bytes.NewReader(contextLengths(maxPending))); err != nil || got != (Info{Architecture: "llama", ContextLength: 2048}) {
		t.Fatalf("as many context lengths as the reader keeps: %+v, %v", got, err)
	}
}

func TestReadRefusesAHeaderCutAnywhere(t *testing.T) {
	headers := [][]byte{
		gguftest.Header(3, gguftest.Strings("tokenizer.ggml.tokens", "<s>"),
			gguftest.Array("tokenizer.ggml.scores", gguftest.TypeFloat32, 1, make([]byte, 4)), arch, window),
		gguftest.Header(3, gguftest.U64("gemma3.context_length", 8192), gguftest.String("general.architecture", "gemma3")),
	}
	for _, h := range headers {
		for n := range len(h) {
			if _, err := read(bytes.NewReader(h[:n])); !errors.Is(err, ErrNotGGUF) && !errors.Is(err, ErrHeader) {
				t.Fatalf("a header cut after %d of its %d bytes: %v", n, len(h), err)
			}
		}
	}
}

func TestReadConsumesAtMostMaxHeaderBytes(t *testing.T) {
	room := MaxHeaderBytes - 1024
	fits := gguftest.Header(3, gguftest.Array("tokenizer.ggml.token_type", gguftest.TypeUint8, uint64(room),
		make([]byte, room)), arch, window)
	if got, err := read(bytes.NewReader(fits)); err != nil || got != (Info{Architecture: "llama", ContextLength: 2048}) {
		t.Fatalf("arrays in front that leave room for the keys: %+v, %v", got, err)
	}
	over := gguftest.Header(3, gguftest.Array("tokenizer.ggml.token_type", gguftest.TypeUint8, MaxHeaderBytes,
		make([]byte, MaxHeaderBytes)), arch, window)
	if _, err := read(bytes.NewReader(over)); !errors.Is(err, ErrHeader) {
		t.Fatalf("keys beyond the first %d bytes are not read: %v", MaxHeaderBytes, err)
	}
	// before is what the reader consumes up to the array's elements: the preamble, the key "x" and its type, and the
	// array's element type and count. The budget then ends inside the next key's length, and inside its name.
	const before = 24 + 8 + 1 + 4 + 4 + 8
	for _, left := range []int{2, 11} {
		n := MaxHeaderBytes - before - left
		h := gguftest.Header(3, gguftest.Array("x", gguftest.TypeUint8, uint64(n), make([]byte, n)), arch, window)
		if _, err := read(bytes.NewReader(h)); !errors.Is(err, ErrHeader) {
			t.Fatalf("a budget that ends %d bytes after the array: %v", left, err)
		}
	}
}

func TestReadInfoReadsAFile(t *testing.T) {
	dir := t.TempDir()
	path := gguftest.WriteModel(t, dir, "m.gguf", "llama", 2048)
	if got, err := ReadInfo(path); err != nil || got != (Info{Architecture: "llama", ContextLength: 2048}) {
		t.Fatalf("a model file: %+v, %v", got, err)
	}
	link := filepath.Join(dir, "link.gguf")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadInfo(link); err != nil || got.ContextLength != 2048 {
		t.Fatalf("a link to a model file, as a local import is: %+v, %v", got, err)
	}
	if _, err := ReadInfo(filepath.Join(dir, "gone.gguf")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file that is gone: %v", err)
	}
	if _, err := ReadInfo(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("a directory: %v", err)
	}
	locked := filepath.Join(dir, "locked.gguf")
	if err := os.WriteFile(locked, gguftest.Model("llama", 2048), 0o000); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 { // root reads anything
		if _, err := ReadInfo(locked); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("a file this user cannot read: %v", err)
		}
	}
}

func TestReadTakesTheFileType(t *testing.T) {
	header := gguftest.Header(3, arch, gguftest.U32("general.file_type", 15), window)
	got, err := read(bytes.NewReader(header))
	if err != nil || !got.HasFileType || got.FileType != 15 || got.Quantization() != "Q4_K_M" {
		t.Fatalf("got %+v, %v; want file type 15, Q4_K_M", got, err)
	}
	if got, err := read(bytes.NewReader(gguftest.Header(3, arch, window))); err != nil || got.HasFileType || got.Quantization() != "" {
		t.Fatalf("a header without a file type: %+v, %v", got, err)
	}
}

func TestQuantizationNamesTheKnownFileTypes(t *testing.T) {
	rows := map[uint32]string{0: "F32", 1: "F16", 2: "Q4_0", 7: "Q8_0", 15: "Q4_K_M", 18: "Q6_K", 19: "IQ2_XXS",
		30: "IQ4_XS", 32: "BF16", 38: "MXFP4_MOE", 39: "NVFP4", 4: "", 33: "", 99: "", 1024: ""}
	for ft, want := range rows {
		if got := (Info{FileType: ft, HasFileType: true}).Quantization(); got != want {
			t.Fatalf("file type %d: %q, want %q", ft, got, want)
		}
	}
}

// realLayout is a header laid out as llama.cpp's converters write it: the architecture, the context length, a large
// tokenizer, and general.file_type last, as every catalog GGUF has it.
func realLayout(tokenizerBytes int, fileType uint32) []byte {
	return gguftest.Header(3, arch, window,
		gguftest.Array("tokenizer.ggml.scores", gguftest.TypeFloat32, uint64(tokenizerBytes/4), make([]byte, tokenizerBytes)),
		gguftest.U32("general.file_type", fileType))
}

func TestReadTakesTheFileTypeAfterALargeTokenizer(t *testing.T) {
	got, err := read(bytes.NewReader(realLayout(8<<20, 15)))
	if err != nil || !got.HasFileType || got.Quantization() != "Q4_K_M" {
		t.Fatalf("the file type after an 8 MiB tokenizer: %+v, %v", got, err)
	}
}

func TestReadWithinTheBudgetKeepsTheTwoKeysWhenTheFileTypeIsPastIt(t *testing.T) {
	got, err := read(bytes.NewReader(realLayout(MaxHeaderBytes, 15)))
	if err != nil || got.HasFileType || got.Architecture != "llama" || got.ContextLength != 2048 {
		t.Fatalf("a file type past the budget: the two keys stay, the quantization is unknown: %+v, %v", got, err)
	}
}

func TestReadStopsAfterTheThreeKeysBeforeACutPair(t *testing.T) {
	header := gguftest.Header(3, arch, gguftest.U32("general.file_type", 7), window, gguftest.String("general.name", "never read"))
	cut := header[:len(header)-5]
	if got, err := read(bytes.NewReader(cut)); err != nil || got.Quantization() != "Q8_0" || got.ContextLength != 2048 {
		t.Fatalf("got %+v, %v", got, err)
	}
}
