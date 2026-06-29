package pluginc

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// typeName maps an engine.Type to its wire name. The wire uses the same lower
// case names engine.Type.String() produces, so the two stay in sync.
func typeFromName(s string) engine.Type {
	switch s {
	case "int":
		return engine.TypeInt
	case "float":
		return engine.TypeFloat
	case "string":
		return engine.TypeString
	case "bool":
		return engine.TypeBool
	case "time":
		return engine.TypeTime
	case "duration":
		return engine.TypeDuration
	case "bytes":
		return engine.TypeBytes
	case "null":
		return engine.TypeNull
	default:
		// Unknown or "any" — treat as untyped structured data.
		return engine.TypeAny
	}
}

// toSchema converts a wire schema into an engine.Schema.
func toSchema(ws wireSchema) engine.Schema {
	cols := make([]engine.Column, len(ws.Columns))
	for i, c := range ws.Columns {
		cols[i] = engine.Column{Name: c.Name, Type: typeFromName(c.Type), Nullable: c.Nullable}
	}
	return engine.Schema{Columns: cols}
}

// decodeCell turns one raw JSON cell into an engine.Value, coerced to the
// column's declared type. JSON null becomes SQL NULL for any type. The string
// encodings (RFC3339 time, base64 bytes, Go-duration or integer-nanos duration)
// mirror what PLUGINS.md asks plugins to emit.
func decodeCell(raw json.RawMessage, typ engine.Type) (engine.Value, error) {
	// A literal JSON null is SQL NULL regardless of column type.
	if len(raw) == 0 || string(raw) == "null" {
		return engine.Null(), nil
	}
	switch typ {
	case engine.TypeInt:
		// Accept a JSON number or a numeric string.
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return engine.Value{}, fmt.Errorf("int cell: %w", err)
		}
		i, err := n.Int64()
		if err != nil {
			// Tolerate "1.0"-style floats that are whole numbers.
			f, ferr := n.Float64()
			if ferr != nil {
				return engine.Value{}, fmt.Errorf("int cell %q: %w", n, err)
			}
			i = int64(f)
		}
		return engine.IntVal(i), nil
	case engine.TypeFloat:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return engine.Value{}, fmt.Errorf("float cell: %w", err)
		}
		return engine.FloatVal(f), nil
	case engine.TypeString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return engine.Value{}, fmt.Errorf("string cell: %w", err)
		}
		return engine.StringVal(s), nil
	case engine.TypeBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return engine.Value{}, fmt.Errorf("bool cell: %w", err)
		}
		return engine.BoolVal(b), nil
	case engine.TypeTime:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return engine.Value{}, fmt.Errorf("time cell: %w", err)
		}
		t, err := parseTime(s)
		if err != nil {
			return engine.Value{}, err
		}
		return engine.TimeVal(t), nil
	case engine.TypeDuration:
		return decodeDuration(raw)
	case engine.TypeBytes:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return engine.Value{}, fmt.Errorf("bytes cell: %w", err)
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return engine.Value{}, fmt.Errorf("bytes cell: %w", err)
		}
		return engine.Value{Type: engine.TypeBytes, V: b}, nil
	default:
		// TypeAny / TypeNull: decode to a generic Go value and let FromAny tag it.
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return engine.Value{}, fmt.Errorf("any cell: %w", err)
		}
		return connector.FromAny(v), nil
	}
}

// parseTime accepts RFC3339 (with or without nanos); both are common plugin
// outputs.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("time cell %q: not RFC3339", s)
	}
	return t, nil
}

// decodeDuration accepts either a Go duration string ("1h30m") or an integer
// number of nanoseconds.
func decodeDuration(raw json.RawMessage) (engine.Value, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		d, err := time.ParseDuration(s)
		if err != nil {
			return engine.Value{}, fmt.Errorf("duration cell %q: %w", s, err)
		}
		return engine.Value{Type: engine.TypeDuration, V: d}, nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return engine.Value{}, fmt.Errorf("duration cell: want string or integer nanos")
	}
	return engine.Value{Type: engine.TypeDuration, V: time.Duration(n)}, nil
}
