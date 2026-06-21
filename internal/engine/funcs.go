package engine

import (
	"fmt"
	"strings"
	"time"
)

// ScalarFunc is a scalar SQL function implementation. It receives the
// already-evaluated argument values and returns a result. Returning an error
// surfaces a message to the user.
type ScalarFunc func(args []Value) (Value, error)

// FuncRegistry holds named scalar functions. Aggregate functions are handled
// separately by the Aggregate operator, not here.
type FuncRegistry struct {
	funcs map[string]ScalarFunc
}

// NewFuncRegistry returns a registry pre-populated with the v0.1 scalar
// function library.
func NewFuncRegistry() *FuncRegistry {
	r := &FuncRegistry{funcs: map[string]ScalarFunc{}}
	r.registerDefaults()
	return r
}

// Lookup returns the function with the given (case-insensitive) name, or nil.
func (r *FuncRegistry) Lookup(name string) ScalarFunc {
	return r.funcs[strings.ToUpper(name)]
}

// Register adds a function under an uppercased name.
func (r *FuncRegistry) Register(name string, fn ScalarFunc) {
	r.funcs[strings.ToUpper(name)] = fn
}

func (r *FuncRegistry) registerDefaults() {
	r.Register("COALESCE", funcCoalesce)
	r.Register("LOWER", funcLower)
	r.Register("UPPER", funcUpper)
	r.Register("LENGTH", funcLength)
	r.Register("LEN", funcLength)
	r.Register("SUBSTR", funcSubstr)
	r.Register("SUBSTRING", funcSubstr)
	r.Register("TRIM", funcTrim)
	r.Register("LTRIM", funcLTrim)
	r.Register("RTRIM", funcRTrim)
	r.Register("CONCAT", funcConcat)
	r.Register("ABS", funcAbs)
	r.Register("ROUND", funcRound)
	r.Register("FLOOR", funcFloor)
	r.Register("CEIL", funcCeil)
	r.Register("CEILING", funcCeil)
	r.Register("REPLACE", funcReplace)
	r.Register("NOW", funcNow)
	r.Register("CURRENT_TIMESTAMP", funcNow)
}

func funcCoalesce(args []Value) (Value, error) {
	for _, a := range args {
		if !a.IsNull() {
			return a, nil
		}
	}
	return Null(), nil
}

func funcLower(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("LOWER expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.ToLower(args[0].AsString())), nil
}

func funcUpper(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("UPPER expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.ToUpper(args[0].AsString())), nil
}

func funcLength(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("LENGTH expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	switch args[0].Type {
	case TypeString:
		return IntVal(int64(len(args[0].V.(string)))), nil
	case TypeBytes:
		return IntVal(int64(len(args[0].V.([]byte)))), nil
	}
	return IntVal(int64(len(args[0].AsString()))), nil
}

func funcSubstr(args []Value) (Value, error) {
	if len(args) < 2 || len(args) > 3 {
		return Value{}, fmt.Errorf("SUBSTR expects 2 or 3 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	s := args[0].AsString()
	start, ok := args[1].AsInt()
	if !ok {
		return Value{}, fmt.Errorf("SUBSTR start must be integer")
	}
	// SQL is 1-based; negative or 0 clamps to start.
	if start < 1 {
		start = 1
	}
	start--
	runes := []rune(s)
	if int(start) > len(runes) {
		return StringVal(""), nil
	}
	end := int64(len(runes))
	if len(args) == 3 {
		ln, ok := args[2].AsInt()
		if !ok {
			return Value{}, fmt.Errorf("SUBSTR length must be integer")
		}
		end = start + ln
	}
	if end < start {
		end = start
	}
	if end > int64(len(runes)) {
		end = int64(len(runes))
	}
	return StringVal(string(runes[start:end])), nil
}

func funcTrim(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("TRIM expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.TrimSpace(args[0].AsString())), nil
}

func funcLTrim(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("LTRIM expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.TrimLeft(args[0].AsString(), " \t\n\r")), nil
}

func funcRTrim(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("RTRIM expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.TrimRight(args[0].AsString(), " \t\n\r")), nil
}

func funcConcat(args []Value) (Value, error) {
	var b strings.Builder
	for _, a := range args {
		if a.IsNull() {
			continue
		}
		b.WriteString(a.AsString())
	}
	return StringVal(b.String()), nil
}

func funcAbs(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("ABS expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	if n, ok := args[0].AsInt(); ok && args[0].Type == TypeInt {
		if n < 0 {
			n = -n
		}
		return IntVal(n), nil
	}
	if f, ok := args[0].AsFloat(); ok {
		return FloatVal(absFloat(f)), nil
	}
	return Value{}, fmt.Errorf("ABS requires numeric")
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func funcRound(args []Value) (Value, error) {
	if len(args) < 1 || len(args) > 2 {
		return Value{}, fmt.Errorf("ROUND expects 1 or 2 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	f, ok := args[0].AsFloat()
	if !ok {
		return Value{}, fmt.Errorf("ROUND requires numeric")
	}
	if len(args) == 1 {
		return FloatVal(roundAt(f, 0)), nil
	}
	d, ok := args[1].AsInt()
	if !ok {
		return Value{}, fmt.Errorf("ROUND digits must be integer")
	}
	return FloatVal(roundAt(f, int(d))), nil
}

func roundAt(f float64, digits int) float64 {
	pow := 1.0
	for i := 0; i < digits; i++ {
		pow *= 10
	}
	return float64(int64(f*pow+0.5)) / pow
}

func funcFloor(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("FLOOR expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	f, ok := args[0].AsFloat()
	if !ok {
		return Value{}, fmt.Errorf("FLOOR requires numeric")
	}
	return FloatVal(float64(int64(f))), nil
}

func funcCeil(args []Value) (Value, error) {
	if len(args) != 1 {
		return Value{}, fmt.Errorf("CEIL expects 1 arg")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	f, ok := args[0].AsFloat()
	if !ok {
		return Value{}, fmt.Errorf("CEIL requires numeric")
	}
	return FloatVal(float64(int64(f) + 1)), nil
}

func funcReplace(args []Value) (Value, error) {
	if len(args) != 3 {
		return Value{}, fmt.Errorf("REPLACE expects 3 args")
	}
	if args[0].IsNull() {
		return Null(), nil
	}
	return StringVal(strings.ReplaceAll(args[0].AsString(), args[1].AsString(), args[2].AsString())), nil
}

func funcNow(args []Value) (Value, error) {
	if len(args) != 0 {
		return Value{}, fmt.Errorf("NOW expects 0 args")
	}
	return TimeVal(time.Now()), nil
}

// IsAggregate reports whether name is a recognized aggregate function.
func IsAggregate(name string) bool {
	switch strings.ToUpper(name) {
	case "COUNT", "SUM", "AVG", "MIN", "MAX":
		return true
	}
	return false
}