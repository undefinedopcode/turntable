package connector

import (
	"time"

	"github.com/april/octoparser/internal/engine"
)

// FromAny converts an arbitrary Go value (as produced by JSON/YAML decoders)
// into an engine.Value, inferring the Type. Used by file connectors.
func FromAny(v any) engine.Value {
	if v == nil {
		return engine.Null()
	}
	switch x := v.(type) {
	case bool:
		return engine.BoolVal(x)
	case int:
		return engine.IntVal(int64(x))
	case int32:
		return engine.IntVal(int64(x))
	case int64:
		return engine.IntVal(x)
	case uint64:
		return engine.IntVal(int64(x))
	case float64:
		return engine.FloatVal(x)
	case float32:
		return engine.FloatVal(float64(x))
	case string:
		return engine.StringVal(x)
	case []byte:
		// JSON/YAML don't produce []byte; treat as string
		return engine.StringVal(string(x))
	case time.Time:
		return engine.TimeVal(x)
	case map[string]any:
		return engine.AnyVal(x)
	case []any:
		return engine.AnyVal(x)
	}
	return engine.AnyVal(v)
}

// InferTypeFrom returns an engine.Type for a sample value.
func InferTypeFrom(v any) engine.Type {
	if v == nil {
		return engine.TypeAny // nullable; type unknown yet
	}
	switch v.(type) {
	case bool:
		return engine.TypeBool
	case int, int32, int64, uint64:
		return engine.TypeInt
	case float32, float64:
		return engine.TypeFloat
	case string:
		return engine.TypeString
	case time.Time:
		return engine.TypeTime
	}
	return engine.TypeAny
}