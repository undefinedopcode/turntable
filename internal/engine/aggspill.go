package engine

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"
)

// This file implements the on-disk side of GROUP BY: a compact binary row codec
// and the partition spilling AggregateIter uses when a single in-memory pass
// would exceed its group budget (see AggConfig). Aggregation stays correct
// because rows are partitioned by group key — every row for a given group hashes
// to the same partition — so each spilled partition can be aggregated
// independently, and the holistic aggregates (which need every value of a group)
// still see the whole group.

// AggConfig bounds the memory a GROUP BY may use. MaxGroups caps how many groups
// a single in-memory aggregation pass may hold (0 = unlimited, the default —
// behaviour identical to a plain hash aggregation). When the cap is reached,
// Spill (if true) partitions the overflow rows to disk under SpillDir and
// aggregates them in later passes; otherwise the query fails with a clear error
// rather than growing until the process is OOM-killed.
//
// The budget bounds group *cardinality*, the common cause of aggregation OOM. It
// does not separately bound a single group's holistic buffer (MEDIAN/PERCENTILE/
// STDDEV/STRING_AGG/DISTINCT retain their argument values); partitioning by key
// cannot split one group, so a single pathologically large group is out of scope
// and should be reduced at the source or with an APPROX_* aggregate.
type AggConfig struct {
	MaxGroups int
	Spill     bool
	SpillDir  string
}

// spillPartitions is how many on-disk partitions a spilling pass fans out into.
// Each recursion re-partitions with a depth-varied hash seed, so the effective
// fan-out compounds and even a very high group cardinality converges in a few
// passes.
const spillPartitions = 16

func init() {
	// TypeAny values (structured/nested data, typically decoded JSON) are
	// gob-encoded when spilled; register the shapes gob cannot infer from an
	// interface value on its own.
	gob.Register(map[string]any{})
	gob.Register([]any{})
}

// ---- Row codec --------------------------------------------------------------

// value type tags for the wire format.
const (
	tagNull byte = iota
	tagInt
	tagFloat
	tagString
	tagBool
	tagTime
	tagDuration
	tagBytes
	tagAny
)

func writeUvarint(w *bufio.Writer, n uint64) error {
	var buf [binary.MaxVarintLen64]byte
	m := binary.PutUvarint(buf[:], n)
	_, err := w.Write(buf[:m])
	return err
}

func writeVarint(w *bufio.Writer, n int64) error {
	var buf [binary.MaxVarintLen64]byte
	m := binary.PutVarint(buf[:], n)
	_, err := w.Write(buf[:m])
	return err
}

func writeLenBytes(w *bufio.Writer, b []byte) error {
	if err := writeUvarint(w, uint64(len(b))); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

func readLenBytes(r *bufio.Reader) ([]byte, error) {
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// encodeValue writes one Value with a leading type tag.
func encodeValue(w *bufio.Writer, v Value) error {
	switch v.Type {
	case TypeNull, TypeInvalid:
		return w.WriteByte(tagNull)
	case TypeInt:
		if err := w.WriteByte(tagInt); err != nil {
			return err
		}
		return writeVarint(w, v.V.(int64))
	case TypeFloat:
		if err := w.WriteByte(tagFloat); err != nil {
			return err
		}
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v.V.(float64)))
		_, err := w.Write(buf[:])
		return err
	case TypeString:
		if err := w.WriteByte(tagString); err != nil {
			return err
		}
		return writeLenBytes(w, []byte(v.V.(string)))
	case TypeBool:
		b := byte(0)
		if v.V.(bool) {
			b = 1
		}
		if err := w.WriteByte(tagBool); err != nil {
			return err
		}
		return w.WriteByte(b)
	case TypeTime:
		bs, err := v.V.(time.Time).MarshalBinary()
		if err != nil {
			return err
		}
		if err := w.WriteByte(tagTime); err != nil {
			return err
		}
		return writeLenBytes(w, bs)
	case TypeDuration:
		if err := w.WriteByte(tagDuration); err != nil {
			return err
		}
		return writeVarint(w, int64(v.V.(time.Duration)))
	case TypeBytes:
		if err := w.WriteByte(tagBytes); err != nil {
			return err
		}
		return writeLenBytes(w, v.V.([]byte))
	case TypeAny:
		if err := w.WriteByte(tagAny); err != nil {
			return err
		}
		if v.V == nil {
			return writeLenBytes(w, nil)
		}
		var buf bytes.Buffer
		if err := gob.NewEncoder(&buf).Encode(&v.V); err != nil {
			return fmt.Errorf("spill: cannot encode structured value: %w", err)
		}
		return writeLenBytes(w, buf.Bytes())
	}
	return fmt.Errorf("spill: unknown value type %v", v.Type)
}

// decodeValue reads one Value written by encodeValue.
func decodeValue(r *bufio.Reader) (Value, error) {
	tag, err := r.ReadByte()
	if err != nil {
		return Value{}, err
	}
	switch tag {
	case tagNull:
		return Null(), nil
	case tagInt:
		n, err := binary.ReadVarint(r)
		return IntVal(n), err
	case tagFloat:
		var buf [8]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return Value{}, err
		}
		return FloatVal(math.Float64frombits(binary.LittleEndian.Uint64(buf[:]))), nil
	case tagString:
		b, err := readLenBytes(r)
		return StringVal(string(b)), err
	case tagBool:
		b, err := r.ReadByte()
		return BoolVal(b == 1), err
	case tagTime:
		b, err := readLenBytes(r)
		if err != nil {
			return Value{}, err
		}
		var t time.Time
		if err := t.UnmarshalBinary(b); err != nil {
			return Value{}, err
		}
		return TimeVal(t), nil
	case tagDuration:
		n, err := binary.ReadVarint(r)
		return Value{Type: TypeDuration, V: time.Duration(n)}, err
	case tagBytes:
		b, err := readLenBytes(r)
		return Value{Type: TypeBytes, V: b}, err
	case tagAny:
		b, err := readLenBytes(r)
		if err != nil {
			return Value{}, err
		}
		if len(b) == 0 {
			return AnyVal(nil), nil
		}
		var val any
		if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&val); err != nil {
			return Value{}, fmt.Errorf("spill: cannot decode structured value: %w", err)
		}
		return AnyVal(val), nil
	}
	return Value{}, fmt.Errorf("spill: unknown value tag %d", tag)
}

// writeRow serializes one row: a value count followed by each value.
func writeRow(w *bufio.Writer, r Row) error {
	if err := writeUvarint(w, uint64(len(r.Values))); err != nil {
		return err
	}
	for _, v := range r.Values {
		if err := encodeValue(w, v); err != nil {
			return err
		}
	}
	return nil
}

// readRow reads one row written by writeRow. ok is false at clean EOF.
func readRow(r *bufio.Reader) (Row, bool, error) {
	n, err := binary.ReadUvarint(r)
	if err == io.EOF {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	vals := make([]Value, n)
	for i := range vals {
		v, err := decodeValue(r)
		if err != nil {
			// A truncated row mid-stream is a real error, not clean EOF.
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return Row{}, false, err
		}
		vals[i] = v
	}
	return Row{Values: vals}, true, nil
}

// ---- Spill partitions -------------------------------------------------------

// spillSet fans a pass's overflow rows out to spillPartitions on-disk files,
// keyed by a hash of the group key so every row of a group lands together.
type spillSet struct {
	dir   string // temp directory owning these partition files
	depth int    // recursion depth, used to vary the partition hash
	files []*spillWriter
}

type spillWriter struct {
	path string
	f    *os.File
	w    *bufio.Writer
}

// newSpillSet creates the partition files under a fresh temp directory below
// baseDir (os.TempDir() when empty).
func newSpillSet(baseDir string, depth int) (*spillSet, error) {
	dir, err := os.MkdirTemp(baseDir, "turntable-agg-*")
	if err != nil {
		return nil, fmt.Errorf("spill: %w", err)
	}
	s := &spillSet{dir: dir, depth: depth, files: make([]*spillWriter, spillPartitions)}
	for i := range s.files {
		path := filepath.Join(dir, fmt.Sprintf("p%02d", i))
		f, err := os.Create(path)
		if err != nil {
			s.cleanup()
			return nil, fmt.Errorf("spill: %w", err)
		}
		s.files[i] = &spillWriter{path: path, f: f, w: bufio.NewWriter(f)}
	}
	return s, nil
}

// partitionFor maps a group key to a partition, mixing the recursion depth into
// the hash so a re-partitioned child pass distributes keys differently.
func partitionFor(key string, depth int) int {
	h := fnv.New64a()
	var seed [8]byte
	binary.LittleEndian.PutUint64(seed[:], uint64(depth)*0x9E3779B97F4A7C15+1)
	h.Write(seed[:])
	io.WriteString(h, key)
	return int(h.Sum64() % uint64(spillPartitions))
}

// write appends a row to the partition its group key hashes to.
func (s *spillSet) write(key string, r Row) error {
	return writeRow(s.files[partitionFor(key, s.depth)].w, r)
}

// finish flushes and closes the writers, returning the non-empty partitions as
// readable sources. Empty partitions are removed immediately.
func (s *spillSet) finish() ([]*spillPartition, error) {
	var parts []*spillPartition
	for _, sw := range s.files {
		if err := sw.w.Flush(); err != nil {
			s.cleanup()
			return nil, fmt.Errorf("spill: %w", err)
		}
		info, err := sw.f.Stat()
		if err != nil {
			s.cleanup()
			return nil, fmt.Errorf("spill: %w", err)
		}
		size := info.Size()
		if err := sw.f.Close(); err != nil {
			s.cleanup()
			return nil, fmt.Errorf("spill: %w", err)
		}
		if size == 0 {
			os.Remove(sw.path)
			continue
		}
		parts = append(parts, &spillPartition{path: sw.path, depth: s.depth + 1})
	}
	// The directory itself is removed when the AggregateIter closes; individual
	// partition files are removed as each is consumed.
	return parts, nil
}

// cleanup removes the whole temp directory (used on error paths).
func (s *spillSet) cleanup() {
	for _, sw := range s.files {
		if sw != nil && sw.f != nil {
			sw.f.Close()
		}
	}
	os.RemoveAll(s.dir)
}

// spillPartition is one on-disk partition of rows awaiting re-aggregation.
type spillPartition struct {
	path  string
	depth int
}

// iterator opens the partition file for a re-aggregation pass. The returned
// iterator removes the file when closed, so consumed partitions do not linger.
func (p *spillPartition) iterator() (RowIterator, error) {
	f, err := os.Open(p.path)
	if err != nil {
		return nil, fmt.Errorf("spill: %w", err)
	}
	return &spillReader{path: p.path, f: f, r: bufio.NewReader(f)}, nil
}

type spillReader struct {
	path string
	f    *os.File
	r    *bufio.Reader
}

func (s *spillReader) Next() (Row, bool, error) { return readRow(s.r) }

func (s *spillReader) Close() error {
	err := s.f.Close()
	os.Remove(s.path) // a partition is read exactly once
	return err
}
