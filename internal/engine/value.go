package engine

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ---- Type coercion ----------------------------------------------------------

// AsBool attempts to interpret v as a boolean (SQL truthiness).
func (v Value) AsBool() (bool, bool) {
	switch v.Type {
	case TypeBool:
		return v.V.(bool), true
	case TypeInt:
		return v.V.(int64) != 0, true
	case TypeFloat:
		return v.V.(float64) != 0, true
	case TypeString:
		s := v.V.(string)
		if strings.EqualFold(s, "true") {
			return true, true
		}
		if strings.EqualFold(s, "false") {
			return false, true
		}
		return false, false
	case TypeNull:
		return false, false
	}
	return false, false
}

// AsInt coerces to int64. Returns ok=false if lossy or impossible.
func (v Value) AsInt() (int64, bool) {
	switch v.Type {
	case TypeInt:
		return v.V.(int64), true
	case TypeFloat:
		f := v.V.(float64)
		if math.Trunc(f) == f && f >= math.MinInt64 && f <= math.MaxInt64 {
			return int64(f), true
		}
		return 0, false
	case TypeBool:
		if v.V.(bool) {
			return 1, true
		}
		return 0, true
	case TypeString:
		n, err := strconv.ParseInt(strings.TrimSpace(v.V.(string)), 10, 64)
		if err == nil {
			return n, true
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(v.V.(string)), 64)
		if err == nil && math.Trunc(f) == f {
			return int64(f), true
		}
		return 0, false
	case TypeNull:
		return 0, false
	}
	return 0, false
}

// AsFloat coerces to float64.
func (v Value) AsFloat() (float64, bool) {
	switch v.Type {
	case TypeFloat:
		return v.V.(float64), true
	case TypeInt:
		return float64(v.V.(int64)), true
	case TypeBool:
		if v.V.(bool) {
			return 1, true
		}
		return 0, true
	case TypeString:
		f, err := strconv.ParseFloat(strings.TrimSpace(v.V.(string)), 64)
		if err == nil {
			return f, true
		}
		return 0, false
	case TypeNull:
		return 0, false
	}
	return 0, false
}

// AsString returns a string representation (always succeeds).
func (v Value) AsString() string {
	if v.IsNull() {
		return ""
	}
	switch v.Type {
	case TypeString:
		return v.V.(string)
	case TypeBytes:
		return string(v.V.([]byte))
	}
	return FormatValue(v)
}

// IsNumeric reports whether the value is int, float, or bool.
func (v Value) IsNumeric() bool {
	switch v.Type {
	case TypeInt, TypeFloat, TypeBool:
		return true
	}
	return false
}

// ---- Comparison -------------------------------------------------------------

// Compare returns -1, 0, +1 for a < b, a == b, a > b using SQL ordering rules.
// NULL ordering: NULLs sort first by default (treated as smallest). Two NULLs
// compare equal. Incomparable types compare by type name for stability.
func Compare(a, b Value) int {
	if a.IsNull() && b.IsNull() {
		return 0
	}
	if a.IsNull() {
		return -1
	}
	if b.IsNull() {
		return 1
	}
	// numeric vs numeric
	if a.IsNumeric() && b.IsNumeric() {
		af, _ := a.AsFloat()
		bf, _ := b.AsFloat()
		switch {
		case af < bf:
			return -1
		case af > bf:
			return 1
		default:
			return 0
		}
	}
	// time vs time
	if a.Type == TypeTime && b.Type == TypeTime {
		ta := a.V.(time.Time)
		tb := b.V.(time.Time)
		switch {
		case ta.Before(tb):
			return -1
		case ta.After(tb):
			return 1
		default:
			return 0
		}
	}
	// duration
	if a.Type == TypeDuration && b.Type == TypeDuration {
		da := a.V.(time.Duration)
		db := b.V.(time.Duration)
		switch {
		case da < db:
			return -1
		case da > db:
			return 1
		default:
			return 0
		}
	}
	// bool vs bool
	if a.Type == TypeBool && b.Type == TypeBool {
		ab, _ := a.AsBool()
		bb, _ := b.AsBool()
		if ab == bb {
			return 0
		}
		if !ab {
			return -1
		}
		return 1
	}
	// fall back to string comparison
	sa, sb := a.AsString(), b.AsString()
	return strings.Compare(sa, sb)
}

// Equal reports whether two values are equal (NULL == NULL is true here; use
// SQL semantics in the evaluator for NULL-aware equality).
func Equal(a, b Value) bool { return Compare(a, b) == 0 }

// ---- Arithmetic ------------------------------------------------------------

// Arith performs a numeric binary op: "+", "-", "*", "/". Returns NULL if either
// side is NULL. Division by zero yields NULL (not a panic).
func Arith(op string, a, b Value) (Value, error) {
	if a.IsNull() || b.IsNull() {
		return Null(), nil
	}
	// Temporal arithmetic when either side is a time or a duration.
	if a.Type == TypeTime || a.Type == TypeDuration || b.Type == TypeTime || b.Type == TypeDuration {
		return temporalArith(op, a, b)
	}
	// If both are ints and op is not "/", keep int.
	ai, aok := a.AsInt()
	bi, bok := b.AsInt()
	if aok && bok && op != "/" {
		switch op {
		case "+":
			return IntVal(ai + bi), nil
		case "-":
			return IntVal(ai - bi), nil
		case "*":
			return IntVal(ai * bi), nil
		}
	}
	af, afok := a.AsFloat()
	bf, bfok := b.AsFloat()
	if !afok || !bfok {
		return Value{}, fmt.Errorf("cannot apply %q to %s and %s", op, a.Type, b.Type)
	}
	switch op {
	case "+":
		return FloatVal(af + bf), nil
	case "-":
		return FloatVal(af - bf), nil
	case "*":
		return FloatVal(af * bf), nil
	case "/":
		if bf == 0 {
			return Null(), nil
		}
		r := af / bf
		if aok && bok && math.Trunc(r) == r {
			return IntVal(int64(r)), nil
		}
		return FloatVal(r), nil
	}
	return Value{}, fmt.Errorf("unknown arithmetic op %q", op)
}

// temporalArith handles +/- on times and durations: time ± duration -> time,
// time - time -> duration, duration ± duration -> duration.
func temporalArith(op string, a, b Value) (Value, error) {
	at, aIsT := a.V.(time.Time)
	bt, bIsT := b.V.(time.Time)
	ad, aIsD := a.V.(time.Duration)
	bd, bIsD := b.V.(time.Duration)
	dur := func(d time.Duration) Value { return Value{Type: TypeDuration, V: d} }
	switch {
	case op == "+" && aIsT && bIsD:
		return TimeVal(at.Add(bd)), nil
	case op == "+" && aIsD && bIsT:
		return TimeVal(bt.Add(ad)), nil
	case op == "-" && aIsT && bIsD:
		return TimeVal(at.Add(-bd)), nil
	case op == "-" && aIsT && bIsT:
		return dur(at.Sub(bt)), nil
	case op == "+" && aIsD && bIsD:
		return dur(ad + bd), nil
	case op == "-" && aIsD && bIsD:
		return dur(ad - bd), nil
	}
	return Value{}, fmt.Errorf("cannot apply %q to %s and %s", op, a.Type, b.Type)
}

// Negate negates a numeric value.
func Negate(v Value) (Value, error) {
	if v.IsNull() {
		return Null(), nil
	}
	if n, ok := v.AsInt(); ok && v.Type == TypeInt {
		return IntVal(-n), nil
	}
	if f, ok := v.AsFloat(); ok {
		return FloatVal(-f), nil
	}
	return Value{}, fmt.Errorf("cannot negate %s", v.Type)
}