package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/april/turntable/internal/sql"
)

// Resolver maps a column reference (qualifier+name) to a position in a row.
// The planner builds a Resolver for each plan node's output schema; the
// evaluator uses it to fetch values. Returning -1 means "not found".
type Resolver func(qualifier, name string) int

// Evaluator evaluates sql.Expr nodes against a row using a Resolver and a
// function registry. It is the core of the expression evaluation used by
// Filter, Project, Aggregate (HAVING), and Sort.
type Evaluator struct {
	Resolve Resolver
	Funcs   *FuncRegistry

	// Strict, when true, makes type-coercion failures return errors instead of
	// NULL. Driven by the CLI --strict flag.
	Strict bool
}

// Eval evaluates a single expression to a Value.
func (e Evaluator) Eval(expr sql.Expr, row Row) (Value, error) {
	switch ex := expr.(type) {
	case *sql.LitInt:
		return IntVal(ex.V), nil
	case *sql.LitFloat:
		return FloatVal(ex.V), nil
	case *sql.LitString:
		return StringVal(ex.V), nil
	case *sql.LitBool:
		return BoolVal(ex.V), nil
	case *sql.LitNull:
		return Null(), nil

	case *sql.ColRef:
		idx := e.Resolve(ex.Qualifier, ex.Name)
		if idx < 0 {
			return Value{}, fmt.Errorf("unknown column %s", colName(ex.Qualifier, ex.Name))
		}
		if idx >= len(row.Values) {
			return Null(), nil
		}
		return row.Values[idx], nil

	case *sql.UnaryOp:
		v, err := e.Eval(ex.Expr, row)
		if err != nil {
			return Value{}, err
		}
		switch ex.Op {
		case "-":
			return Negate(v)
		case "NOT":
			if v.IsNull() {
				return Null(), nil
			}
			b, ok := v.AsBool()
			if !ok {
				return Value{}, fmt.Errorf("NOT requires bool, got %s", v.Type)
			}
			return BoolVal(!b), nil
		}
		return Value{}, fmt.Errorf("unknown unary op %q", ex.Op)

	case *sql.BinaryOp:
		return e.evalBinary(ex, row)

	case *sql.InExpr:
		return e.evalIn(ex, row)
	case *sql.BetweenExpr:
		return e.evalBetween(ex, row)
	case *sql.LikeExpr:
		return e.evalLike(ex, row)
	case *sql.IsNullExpr:
		v, err := e.Eval(ex.Expr, row)
		if err != nil {
			return Value{}, err
		}
		isNull := v.IsNull()
		if ex.Negate {
			return BoolVal(!isNull), nil
		}
		return BoolVal(isNull), nil

	case *sql.FuncCall:
		return e.evalFunc(ex, row)

	case *sql.CaseExpr:
		for _, w := range ex.Whens {
			cond, err := e.Eval(w.Cond, row)
			if err != nil {
				return Value{}, err
			}
			if b, ok := cond.AsBool(); ok && b {
				return e.Eval(w.Then, row)
			}
		}
		if ex.Else != nil {
			return e.Eval(ex.Else, row)
		}
		return Null(), nil

	case *sql.CastExpr:
		v, err := e.Eval(ex.Expr, row)
		if err != nil {
			return Value{}, err
		}
		return castWithMode(v, ex.Type, e.Strict)

	case *sql.ExtractExpr:
		v, err := e.Eval(ex.Source, row)
		if err != nil {
			return Value{}, err
		}
		return extractField(v, ex.Field)

	case *sql.PositionExpr:
		sub, err := e.Eval(ex.Substr, row)
		if err != nil {
			return Value{}, err
		}
		str, err := e.Eval(ex.Str, row)
		if err != nil {
			return Value{}, err
		}
		if sub.IsNull() || str.IsNull() {
			return Null(), nil
		}
		idx := strings.Index(str.AsString(), sub.AsString())
		return IntVal(int64(idx + 1)), nil // 1-based; 0 if not found
	}
	return Value{}, fmt.Errorf("unsupported expression %T", expr)
}

func (e Evaluator) evalBinary(ex *sql.BinaryOp, row Row) (Value, error) {
	// Boolean short-circuit operators with NULL propagation.
	switch ex.Op {
	case "AND":
		l, err := e.Eval(ex.Left, row)
		if err != nil {
			return Value{}, err
		}
		if lb, ok := l.AsBool(); ok && !lb {
			return BoolVal(false), nil
		}
		r, err := e.Eval(ex.Right, row)
		if err != nil {
			return Value{}, err
		}
		if l.IsNull() || r.IsNull() {
			if lb, ok := l.AsBool(); ok && !lb {
				return BoolVal(false), nil
			}
			if rb, ok := r.AsBool(); ok && !rb {
				return BoolVal(false), nil
			}
			return Null(), nil
		}
		lb, _ := l.AsBool()
		rb, _ := r.AsBool()
		return BoolVal(lb && rb), nil
	case "OR":
		l, err := e.Eval(ex.Left, row)
		if err != nil {
			return Value{}, err
		}
		if lb, ok := l.AsBool(); ok && lb {
			return BoolVal(true), nil
		}
		r, err := e.Eval(ex.Right, row)
		if err != nil {
			return Value{}, err
		}
		if l.IsNull() || r.IsNull() {
			if rb, ok := r.AsBool(); ok && rb {
				return BoolVal(true), nil
			}
			return Null(), nil
		}
		lb, _ := l.AsBool()
		rb, _ := r.AsBool()
		return BoolVal(lb || rb), nil
	}

	l, err := e.Eval(ex.Left, row)
	if err != nil {
		return Value{}, err
	}
	r, err := e.Eval(ex.Right, row)
	if err != nil {
		return Value{}, err
	}

	switch ex.Op {
	case "+", "-", "*", "/":
		return Arith(ex.Op, l, r)
	case "=":
		return cmpEquals(l, r), nil
	case "<>":
		v := cmpEquals(l, r)
		if v.IsNull() {
			return v, nil
		}
		return BoolVal(!v.V.(bool)), nil
	case "<", "<=", ">", ">=":
		if l.IsNull() || r.IsNull() {
			return Null(), nil
		}
		c := Compare(l, r)
		switch ex.Op {
		case "<":
			return BoolVal(c < 0), nil
		case "<=":
			return BoolVal(c <= 0), nil
		case ">":
			return BoolVal(c > 0), nil
		case ">=":
			return BoolVal(c >= 0), nil
		}
	}
	return Value{}, fmt.Errorf("unknown binary op %q", ex.Op)
}

// cmpEquals implements SQL "=" with NULL semantics: NULL = anything is NULL.
func cmpEquals(l, r Value) Value {
	if l.IsNull() || r.IsNull() {
		return Null()
	}
	return BoolVal(Compare(l, r) == 0)
}

func (e Evaluator) evalIn(ex *sql.InExpr, row Row) (Value, error) {
	v, err := e.Eval(ex.Expr, row)
	if err != nil {
		return Value{}, err
	}
	if v.IsNull() {
		return Null(), nil
	}
	for _, item := range ex.List {
		iv, err := e.Eval(item, row)
		if err != nil {
			return Value{}, err
		}
		if iv.IsNull() {
			continue
		}
		if Compare(v, iv) == 0 {
			if ex.Negate {
				return BoolVal(false), nil
			}
			return BoolVal(true), nil
		}
	}
	if ex.Negate {
		return BoolVal(true), nil
	}
	return BoolVal(false), nil
}

func (e Evaluator) evalBetween(ex *sql.BetweenExpr, row Row) (Value, error) {
	v, err := e.Eval(ex.Expr, row)
	if err != nil {
		return Value{}, err
	}
	lo, err := e.Eval(ex.Low, row)
	if err != nil {
		return Value{}, err
	}
	hi, err := e.Eval(ex.High, row)
	if err != nil {
		return Value{}, err
	}
	if v.IsNull() || lo.IsNull() || hi.IsNull() {
		return Null(), nil
	}
	in := Compare(v, lo) >= 0 && Compare(v, hi) <= 0
	if ex.Negate {
		return BoolVal(!in), nil
	}
	return BoolVal(in), nil
}

func (e Evaluator) evalLike(ex *sql.LikeExpr, row Row) (Value, error) {
	v, err := e.Eval(ex.Expr, row)
	if err != nil {
		return Value{}, err
	}
	p, err := e.Eval(ex.Pat, row)
	if err != nil {
		return Value{}, err
	}
	if v.IsNull() || p.IsNull() {
		return Null(), nil
	}
	matched := likeMatch(v.AsString(), p.AsString())
	if ex.Negate {
		return BoolVal(!matched), nil
	}
	return BoolVal(matched), nil
}

// likeMatch implements SQL LIKE: % matches any sequence, _ matches one char.
// The match is case-insensitive (a pragmatic choice for v0.1).
func likeMatch(s, pattern string) bool {
	return likeRunes([]rune(strings.ToLower(s)), []rune(strings.ToLower(pattern)))
}

func likeRunes(s, p []rune) bool {
	for len(p) > 0 {
		switch p[0] {
		case '%':
			// collapse multiple %
			for len(p) > 0 && p[0] == '%' {
				p = p[1:]
			}
			if len(p) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if likeRunes(s[i:], p) {
					return true
				}
			}
			return false
		case '_':
			if len(s) == 0 {
				return false
			}
			s, p = s[1:], p[1:]
		default:
			if len(s) == 0 || s[0] != p[0] {
				return false
			}
			s, p = s[1:], p[1:]
		}
	}
	return len(s) == 0
}

func (e Evaluator) evalFunc(ex *sql.FuncCall, row Row) (Value, error) {
	if e.Funcs == nil {
		return Value{}, fmt.Errorf("no function registry; cannot call %s", ex.Name)
	}
	fn := e.Funcs.Lookup(ex.Name)
	if fn == nil {
		return Value{}, fmt.Errorf("unknown function %s", ex.Name)
	}
	args := make([]Value, len(ex.Args))
	for i, a := range ex.Args {
		v, err := e.Eval(a, row)
		if err != nil {
			return Value{}, err
		}
		args[i] = v
	}
	return fn(args)
}

func colName(q, n string) string {
	if q == "" {
		return n
	}
	return q + "." + n
}

// Cast converts v to the named SQL type. Supported: int, float, string, bool,
// time. Unrecognized types return an error. A NULL input yields NULL. Coercion
// failures yield NULL (lenient); use castWithMode(v, t, true) for strict errors.
func Cast(v Value, typ string) (Value, error) {
	return castWithMode(v, typ, false)
}

// castWithMode is the strict-aware implementation of Cast. When strict is true,
// a value that cannot be coerced to the target type produces an error rather
// than NULL.
func castWithMode(v Value, typ string, strict bool) (Value, error) {
	if v.IsNull() {
		return Null(), nil
	}
	coerceErr := func(msg string) (Value, error) {
		if strict {
			return Value{}, fmt.Errorf("CAST: %s", msg)
		}
		return Null(), nil
	}
	switch strings.ToLower(typ) {
	case "int", "integer", "bigint":
		if n, ok := v.AsInt(); ok {
			return IntVal(n), nil
		}
		// Truncate floats toward zero (SQL CAST semantics).
		if f, ok := v.AsFloat(); ok {
			return IntVal(int64(f)), nil
		}
		return coerceErr(fmt.Sprintf("cannot cast %s to int", v.Type))
	case "float", "real", "double":
		f, ok := v.AsFloat()
		if !ok {
			return coerceErr(fmt.Sprintf("cannot cast %s to float", v.Type))
		}
		return FloatVal(f), nil
	case "string", "text", "varchar":
		return StringVal(v.AsString()), nil
	case "bool", "boolean":
		b, ok := v.AsBool()
		if !ok {
			return coerceErr(fmt.Sprintf("cannot cast %s to bool", v.Type))
		}
		return BoolVal(b), nil
	case "time", "timestamp", "datetime":
		if v.Type == TypeTime {
			return v, nil
		}
		s := v.AsString()
		t, err := parseTime(s)
		if err != nil {
			return coerceErr(fmt.Sprintf("cannot parse %q as time", s))
		}
		return TimeVal(t), nil
	}
	return Value{}, fmt.Errorf("unknown type %q", typ)
}

func parseTime(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05.999999",
		"2006-01-02T15:04:05",
		"2006-01-02",
		"2006/01/02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("could not parse time %q", s)
}

// asTime coerces a Value to a time.Time. Strings are parsed; time values pass
// through. Returns ok=false if not coercible.
func asTime(v Value) (time.Time, bool) {
	if v.IsNull() {
		return time.Time{}, false
	}
	if v.Type == TypeTime {
		return v.V.(time.Time), true
	}
	t, err := parseTime(v.AsString())
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// extractField implements EXTRACT(field FROM source). NULL source yields NULL.
// Numeric sources are interpreted as epoch seconds when the field is EPOCH.
func extractField(v Value, field string) (Value, error) {
	if v.IsNull() {
		return Null(), nil
	}
	field = strings.ToUpper(field)
	// Allow EXTRACT(EPOCH FROM <number>) to interpret as unix seconds.
	if field == "EPOCH" {
		if t, ok := asTime(v); ok {
			return FloatVal(float64(t.UnixNano()) / 1e9), nil
		}
		if f, ok := v.AsFloat(); ok {
			return FloatVal(f), nil
		}
		return Null(), nil
	}
	t, ok := asTime(v)
	if !ok {
		return Null(), nil
	}
	switch field {
	case "YEAR":
		return IntVal(int64(t.Year())), nil
	case "MONTH":
		return IntVal(int64(t.Month())), nil
	case "DAY":
		return IntVal(int64(t.Day())), nil
	case "HOUR":
		return IntVal(int64(t.Hour())), nil
	case "MINUTE":
		return IntVal(int64(t.Minute())), nil
	case "SECOND":
		return FloatVal(float64(t.Second()) + float64(t.Nanosecond())/1e9), nil
	case "DOW":
		// 0=Sunday .. 6=Saturday (Postgres-style is 0=Sunday).
		return IntVal(int64(t.Weekday())), nil
	case "DOY":
		return IntVal(int64(t.YearDay())), nil
	}
	return Value{}, fmt.Errorf("unknown EXTRACT field %q", field)
}