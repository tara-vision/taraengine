// Package gguftest writes GGUF headers for tests: the pairs given, in the order given, little-endian, with no
// tensors. It does not import the reader, whose own tests use it.
package gguftest

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// The value types of a GGUF key, as the format numbers them.
const (
	TypeUint8 uint32 = iota
	TypeInt8
	TypeUint16
	TypeInt16
	TypeUint32
	TypeInt32
	TypeFloat32
	TypeBool
	TypeString
	TypeArray
	TypeUint64
	TypeInt64
	TypeFloat64
)

// magic is "GGUF" as the file stores it.
const magic = "GGUF"

// KV is one key-value pair: the key, the value type and the bytes of the value as they follow the type in a file.
type KV struct {
	Key   string
	Type  uint32
	Value []byte
}

// String is a string value.
func String(key, value string) KV { return KV{Key: key, Type: TypeString, Value: text(value)} }

// U32 is a uint32 value.
func U32(key string, v uint32) KV {
	return KV{Key: key, Type: TypeUint32, Value: binary.LittleEndian.AppendUint32(nil, v)}
}

// U64 is a uint64 value.
func U64(key string, v uint64) KV {
	return KV{Key: key, Type: TypeUint64, Value: binary.LittleEndian.AppendUint64(nil, v)}
}

// I32 is an int32 value.
func I32(key string, v int32) KV {
	bits := uint32(v) //nolint:gosec // G115: the two's complement bits, as the format stores them
	return KV{Key: key, Type: TypeInt32, Value: binary.LittleEndian.AppendUint32(nil, bits)}
}

// F32 is a float32 value.
func F32(key string, v float32) KV {
	return KV{Key: key, Type: TypeFloat32, Value: binary.LittleEndian.AppendUint32(nil, math.Float32bits(v))}
}

// Strings is an array of strings.
func Strings(key string, values ...string) KV {
	v := binary.LittleEndian.AppendUint32(nil, TypeString)
	v = binary.LittleEndian.AppendUint64(v, uint64(len(values)))
	for _, s := range values {
		v = append(v, text(s)...)
	}
	return KV{Key: key, Type: TypeArray, Value: v}
}

// Array is an array that claims count elements of the type elem and carries elems as their bytes, which may be fewer
// than the count claims, or none at all.
func Array(key string, elem uint32, count uint64, elems []byte) KV {
	v := binary.LittleEndian.AppendUint32(nil, elem)
	v = binary.LittleEndian.AppendUint64(v, count)
	return KV{Key: key, Type: TypeArray, Value: append(v, elems...)}
}

// Header is a GGUF header of the version: the magic, the version, no tensors, the number of pairs and the pairs.
func Header(version uint32, kvs ...KV) []byte {
	b := append([]byte(nil), magic...)
	b = binary.LittleEndian.AppendUint32(b, version)
	b = binary.LittleEndian.AppendUint64(b, 0)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(kvs)))
	for _, kv := range kvs {
		b = append(b, text(kv.Key)...)
		b = binary.LittleEndian.AppendUint32(b, kv.Type)
		b = append(b, kv.Value...)
	}
	return b
}

// Model is a version 3 header of a model of the architecture, trained for contextLength tokens, with a tokenizer
// list in front of the two keys the engine reads, as some converters write it.
func Model(arch string, contextLength uint32) []byte {
	return Header(3, Strings("tokenizer.ggml.tokens", "<unk>", "<s>", "</s>"),
		String("general.architecture", arch), U32(arch+".context_length", contextLength))
}

// WriteModel writes Model(arch, contextLength) to dir/name and returns the path. A header it cannot write fails the
// test.
func WriteModel(t testing.TB, dir, name, arch string, contextLength uint32) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, Model(arch, contextLength), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// text is a string as the format stores it: its length, then its bytes.
func text(s string) []byte {
	return append(binary.LittleEndian.AppendUint64(nil, uint64(len(s))), s...)
}
