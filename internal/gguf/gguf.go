// Package gguf reads the little the engine needs from the header of a GGUF model file: the architecture and the
// context window the model was trained for. The file can come from anywhere, so the reader consumes at most
// MaxHeaderBytes, read or skipped, and checks every length and count against what is left before it reads or
// skips anything.
package gguf

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// MaxHeaderBytes is the most of a file the reader consumes. The keys it wants come long before the tensor data; a
// tokenizer's arrays may come first, as in stories260K, and fit many times over.
const MaxHeaderBytes = 16 << 20

const (
	// magic is "GGUF" read as a little-endian uint32. A big-endian file reads its version as another number.
	magic uint32 = 0x46554747
	// maxKeyBytes bounds the keys the reader holds; a longer key is skipped, since no key it wants is that long.
	maxKeyBytes = 256
	// maxArchBytes bounds the name of an architecture.
	maxArchBytes = 64
	// maxPending bounds the context lengths kept while the architecture is not known yet.
	maxPending = 32
	// keyArchitecture and contextSuffix name the two keys the reader wants.
	keyArchitecture = "general.architecture"
	contextSuffix   = ".context_length"
)

// The value types of a GGUF key, as the format numbers them.
const (
	typeUint8 uint32 = iota
	typeInt8
	typeUint16
	typeInt16
	typeUint32
	typeInt32
	typeFloat32
	typeBool
	typeString
	typeArray
	typeUint64
	typeInt64
	typeFloat64
)

// The errors ReadInfo returns, wrapped; errors.Is tells them apart.
var (
	// ErrNotGGUF is a file that does not start as a little-endian GGUF file of version 2 or 3.
	ErrNotGGUF = errors.New("gguf: not a little-endian GGUF file of version 2 or 3")
	// ErrHeader is a header the reader cannot walk: cut short, longer than the reader takes, or holding a value it
	// refuses.
	ErrHeader = errors.New("gguf: the header cannot be read")
	// ErrMissing is a header without the architecture, or without its context length.
	ErrMissing = errors.New("gguf: the header names no architecture or no context length")
)

// Info is what the header says about the model. ContextLength is <architecture>.context_length as the file stores
// it; the caller decides which values it takes.
type Info struct {
	Architecture  string
	ContextLength uint64
}

// ReadInfo reads the architecture and the native context length from the header of the GGUF file at path. It opens
// only a regular file, so that a named pipe at the path cannot block it, and it stops as soon as it has both keys,
// in whatever order the file holds them.
func ReadInfo(path string) (Info, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, fmt.Errorf("gguf: %w", err)
	}
	if !st.Mode().IsRegular() {
		return Info{}, fmt.Errorf("gguf: %s is not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: a model file the engine pulled or was given, read and never run
	if err != nil {
		return Info{}, fmt.Errorf("gguf: %w", err)
	}
	defer func() { _ = f.Close() }()
	return read(f)
}

// read walks the header from the start of r.
func read(r io.Reader) (Info, error) {
	d := &decoder{r: bufio.NewReader(r), left: MaxHeaderBytes}
	pairs, err := d.preamble()
	if err != nil {
		return Info{}, err
	}
	var s search
	for range pairs {
		if err := d.pair(&s); err != nil {
			return Info{}, err
		}
		if s.arch != "" && s.hasLength {
			return Info{Architecture: s.arch, ContextLength: s.length}, nil
		}
	}
	return Info{}, ErrMissing
}

// search is what the walk has found so far. Context lengths seen before the architecture wait in pending, by the
// prefix of their key.
type search struct {
	arch      string
	length    uint64
	hasLength bool
	pending   map[string]uint64
}

// decoder reads little-endian values and counts what it consumes against the budget.
type decoder struct {
	r    *bufio.Reader
	left uint64
	buf  [8]byte
}

// preamble reads the magic, the version and the tensor count, and returns the number of key-value pairs.
func (d *decoder) preamble() (uint64, error) {
	m, err := d.u32()
	if err != nil || m != magic {
		return 0, ErrNotGGUF
	}
	version, err := d.u32()
	if err != nil {
		return 0, err
	}
	if version != 2 && version != 3 {
		return 0, ErrNotGGUF
	}
	if _, err := d.u64(); err != nil { // the tensor count
		return 0, err
	}
	return d.u64()
}

// pair reads one key-value pair and keeps it when it is one the search wants.
func (d *decoder) pair(s *search) error {
	key, held, err := d.key()
	if err != nil {
		return err
	}
	typ, err := d.u32()
	if err != nil {
		return err
	}
	switch {
	case held && key == keyArchitecture && typ == typeString:
		return d.architecture(s)
	case held && strings.HasSuffix(key, contextSuffix) && (typ == typeUint32 || typ == typeUint64):
		return d.contextLength(s, strings.TrimSuffix(key, contextSuffix), typ)
	}
	return d.skipValue(typ)
}

// key reads a key. One longer than any key the search wants is skipped, and held is false.
func (d *decoder) key() (key string, held bool, err error) {
	n, err := d.u64()
	if err != nil {
		return "", false, err
	}
	if n > maxKeyBytes {
		return "", false, d.skip(n)
	}
	b, err := d.bytes(n)
	return string(b), err == nil, err
}

// architecture reads the architecture's name and takes the context length that was waiting for it.
func (d *decoder) architecture(s *search) error {
	n, err := d.u64()
	if err != nil {
		return err
	}
	if n == 0 || n > maxArchBytes {
		return fmt.Errorf("%w: an architecture name of %d bytes", ErrHeader, n)
	}
	b, err := d.bytes(n)
	if err != nil {
		return err
	}
	s.arch = string(b)
	if v, ok := s.pending[s.arch]; ok {
		s.length, s.hasLength = v, true
	}
	return nil
}

// contextLength reads a context length. Once the architecture is known only its own counts; before, each one waits
// under the prefix of its key.
func (d *decoder) contextLength(s *search, prefix string, typ uint32) error {
	var v uint64
	if typ == typeUint32 {
		v32, err := d.u32()
		if err != nil {
			return err
		}
		v = uint64(v32)
	} else {
		v64, err := d.u64()
		if err != nil {
			return err
		}
		v = v64
	}
	switch {
	case s.arch != "":
		if prefix == s.arch {
			s.length, s.hasLength = v, true
		}
	case len(s.pending) >= maxPending:
		return fmt.Errorf("%w: more than %d context lengths before the architecture", ErrHeader, maxPending)
	default:
		if s.pending == nil {
			s.pending = map[string]uint64{}
		}
		s.pending[prefix] = v
	}
	return nil
}

// skipValue skips a value of the type without holding it.
func (d *decoder) skipValue(typ uint32) error {
	if size := fixedSize(typ); size > 0 {
		return d.skip(size)
	}
	switch typ {
	case typeString:
		return d.skipString()
	case typeArray:
		return d.skipArray()
	}
	return fmt.Errorf("%w: a value of the unknown type %d", ErrHeader, typ)
}

func (d *decoder) skipString() error {
	n, err := d.u64()
	if err != nil {
		return err
	}
	return d.skip(n)
}

// skipArray skips an array of fixed-size values in one step and an array of strings one string at a time. An array
// of arrays is refused, as llama.cpp refuses it.
func (d *decoder) skipArray() error {
	elem, err := d.u32()
	if err != nil {
		return err
	}
	count, err := d.u64()
	if err != nil {
		return err
	}
	if size := fixedSize(elem); size > 0 {
		if count > d.left/size {
			return d.overBudget()
		}
		return d.skip(count * size)
	}
	switch elem {
	case typeString:
		for range count {
			if err := d.skipString(); err != nil {
				return err
			}
		}
		return nil
	case typeArray:
		return fmt.Errorf("%w: an array of arrays", ErrHeader)
	}
	return fmt.Errorf("%w: an array of the unknown type %d", ErrHeader, elem)
}

// fixedSize is the size in bytes of a value of a fixed-size type, and 0 for a string, an array or a type the format
// does not have.
func fixedSize(typ uint32) uint64 {
	switch typ {
	case typeUint8, typeInt8, typeBool:
		return 1
	case typeUint16, typeInt16:
		return 2
	case typeUint32, typeInt32, typeFloat32:
		return 4
	case typeUint64, typeInt64, typeFloat64:
		return 8
	}
	return 0
}

func (d *decoder) overBudget() error {
	return fmt.Errorf("%w: it goes on past the first %d bytes, which is all the reader takes", ErrHeader, MaxHeaderBytes)
}

// take counts n bytes against the budget.
func (d *decoder) take(n uint64) error {
	if n > d.left {
		return d.overBudget()
	}
	d.left -= n
	return nil
}

// fill reads the next n bytes, at most eight, into the scratch buffer.
func (d *decoder) fill(n uint64) error {
	if err := d.take(n); err != nil {
		return err
	}
	if _, err := io.ReadFull(d.r, d.buf[:n]); err != nil {
		return fmt.Errorf("%w: it ends early (%w)", ErrHeader, err)
	}
	return nil
}

func (d *decoder) u32() (uint32, error) {
	if err := d.fill(4); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(d.buf[:4]), nil
}

func (d *decoder) u64() (uint64, error) {
	if err := d.fill(8); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(d.buf[:8]), nil
}

// bytes reads the next n bytes; the callers bound n first.
func (d *decoder) bytes(n uint64) ([]byte, error) {
	if err := d.take(n); err != nil {
		return nil, err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(d.r, b); err != nil {
		return nil, fmt.Errorf("%w: it ends early (%w)", ErrHeader, err)
	}
	return b, nil
}

// skip passes over the next n bytes without holding them.
func (d *decoder) skip(n uint64) error {
	if n > d.left || n > MaxHeaderBytes {
		return d.overBudget()
	}
	d.left -= n
	if _, err := io.CopyN(io.Discard, d.r, int64(n)); err != nil {
		return fmt.Errorf("%w: it ends early (%w)", ErrHeader, err)
	}
	return nil
}
