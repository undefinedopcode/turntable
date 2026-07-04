// Package engine defines the in-memory query execution primitives: types,
// schemas, rows, and the operator pipeline that executes a logical plan.
//
// This file holds the foundational type system shared across the parser,
// planner, connectors, and renderers.
package engine

import (
	"fmt"
	"time"
)

// Type is the runtime type tag for a column value.
type Type int

const (
	TypeInvalid Type = iota
	TypeNull
	TypeInt     // int64
	TypeFloat   // float64
	TypeString  // string
	TypeBool    // bool
	TypeTime    // time.Time
	TypeDuration
	TypeBytes
	TypeAny // untyped/structured (nested objects/arrays)
)

func (t Type) String() string {
	switch t {
	case TypeInvalid:
		return "invalid"
	case TypeNull:
		return "null"
	case TypeInt:
		return "int"
	case TypeFloat:
		return "float"
	case TypeString:
		return "string"
	case TypeBool:
		return "bool"
	case TypeTime:
		return "time"
	case TypeDuration:
		return "duration"
	case TypeBytes:
		return "bytes"
	case TypeAny:
		return "any"
	}
	return "unknown"
}

// TypeFromString is the inverse of Type.String(): it maps a type name back to a
// Type, reporting whether the name was recognized. Handy for reconstructing a
// schema from serialized column type names (e.g. the web export endpoint).
func TypeFromString(s string) (Type, bool) {
	switch s {
	case "null":
		return TypeNull, true
	case "int":
		return TypeInt, true
	case "float":
		return TypeFloat, true
	case "string":
		return TypeString, true
	case "bool":
		return TypeBool, true
	case "time":
		return TypeTime, true
	case "duration":
		return TypeDuration, true
	case "bytes":
		return TypeBytes, true
	case "any":
		return TypeAny, true
	}
	return TypeInvalid, false
}

// Value is a single typed cell. The concrete Go type corresponds to Type.
type Value struct {
	Type Type
	V    any
}

// Common constructors.
func Null() Value             { return Value{Type: TypeNull} }
func IntVal(v int64) Value    { return Value{Type: TypeInt, V: v} }
func FloatVal(v float64) Value { return Value{Type: TypeFloat, V: v} }
func StringVal(v string) Value { return Value{Type: TypeString, V: v} }
func BoolVal(v bool) Value    { return Value{Type: TypeBool, V: v} }
func TimeVal(v time.Time) Value { return Value{Type: TypeTime, V: v} }
func AnyVal(v any) Value       { return Value{Type: TypeAny, V: v} }

// IsNull reports whether the value is SQL NULL.
func (v Value) IsNull() bool { return v.Type == TypeNull }

// Column describes a column in a Schema.
type Column struct {
	Name     string
	Type     Type
	Nullable bool
}

// Schema is the ordered set of columns for a relation.
type Schema struct {
	Columns []Column
}

// Index returns the position of a column by (case-insensitive) name, or -1.
func (s Schema) Index(name string) int {
	for i, c := range s.Columns {
		if equalFold(c.Name, name) {
			return i
		}
	}
	return -1
}

// Row is a sequence of Values aligned to a Schema.
type Row struct {
	Values []Value
}

// RowIterator streams rows from a connector or operator.
type RowIterator interface {
	// Next returns the next row. ok is false at EOF. err is non-nil on failure.
	Next() (Row, bool, error)
	Close() error
}

// SliceIter adapts an in-memory slice of rows into a RowIterator.
type SliceIter struct {
	rows []Row
	i    int
}

func NewSliceIter(rows []Row) *SliceIter { return &SliceIter{rows: rows} }

func (s *SliceIter) Next() (Row, bool, error) {
	if s.i >= len(s.rows) {
		return Row{}, false, nil
	}
	r := s.rows[s.i]
	s.i++
	return r, true, nil
}

func (s *SliceIter) Close() error { return nil }

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// FormatValue returns a human-readable string for a Value, used by renderers.
func FormatValue(v Value) string {
	if v.IsNull() {
		return ""
	}
	switch v.Type {
	case TypeInt, TypeFloat, TypeString, TypeBool:
		return fmt.Sprintf("%v", v.V)
	case TypeTime:
		return v.V.(time.Time).Format(time.RFC3339)
	case TypeDuration:
		return v.V.(time.Duration).String()
	case TypeBytes:
		return string(v.V.([]byte))
	case TypeAny:
		return fmt.Sprintf("%v", v.V)
	}
	return ""
}