package engine

import (
	"testing"
	"time"

	"github.com/april/turntable/internal/sql"
)

func TestCompareNumerics(t *testing.T) {
	cases := []struct {
		a, b Value
		want int
	}{
		{IntVal(1), IntVal(2), -1},
		{IntVal(2), IntVal(1), 1},
		{IntVal(1), IntVal(1), 0},
		{FloatVal(1.5), FloatVal(2.5), -1},
		{IntVal(1), FloatVal(1.0), 0},
		{Null(), IntVal(1), -1},
		{IntVal(1), Null(), 1},
		{Null(), Null(), 0},
		{StringVal("a"), StringVal("b"), -1},
		{BoolVal(false), BoolVal(true), -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestArith(t *testing.T) {
	cases := []struct {
		op   string
		a, b Value
		want Value
	}{
		{"+", IntVal(1), IntVal(2), IntVal(3)},
		{"-", IntVal(5), IntVal(3), IntVal(2)},
		{"*", IntVal(3), IntVal(4), IntVal(12)},
		{"/", IntVal(6), IntVal(2), IntVal(3)},
		{"/", IntVal(1), IntVal(0), Null()},
		{"+", IntVal(1), Null(), Null()},
		{"+", FloatVal(1.5), FloatVal(2.5), FloatVal(4)},
	}
	for _, c := range cases {
		got, err := Arith(c.op, c.a, c.b)
		if err != nil {
			t.Fatalf("Arith(%s,%v,%v) error: %v", c.op, c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("Arith(%s,%v,%v) = %v, want %v", c.op, c.a, c.b, got, c.want)
		}
	}
}

func TestEvaluator_LiteralsAndCols(t *testing.T) {
	schema := Schema{Columns: []Column{
		{Name: "id", Type: TypeInt},
		{Name: "name", Type: TypeString},
	}}
	resolver := SchemaResolver(schema, "users")
	eval := Evaluator{Resolve: resolver, Funcs: NewFuncRegistry()}
	row := Row{Values: []Value{IntVal(7), StringVal("Al")}}

	cases := []struct {
		expr sql.Expr
		want Value
	}{
		{&sql.LitInt{V: 3}, IntVal(3)},
		{&sql.LitString{V: "x"}, StringVal("x")},
		{&sql.ColRef{Name: "id"}, IntVal(7)},
		{&sql.ColRef{Qualifier: "users", Name: "name"}, StringVal("Al")},
		{&sql.BinaryOp{Op: "+", Left: &sql.ColRef{Name: "id"}, Right: &sql.LitInt{V: 1}}, IntVal(8)},
		{&sql.BinaryOp{Op: "=", Left: &sql.ColRef{Name: "id"}, Right: &sql.LitInt{V: 7}}, BoolVal(true)},
		{&sql.BinaryOp{Op: "AND", Left: &sql.LitBool{V: true}, Right: &sql.LitBool{V: false}}, BoolVal(false)},
	}
	for _, c := range cases {
		got, err := eval.Eval(c.expr, row)
		if err != nil {
			t.Fatalf("Eval(%T) error: %v", c.expr, err)
		}
		if got != c.want {
			t.Errorf("Eval(%T) = %v, want %v", c.expr, got, c.want)
		}
	}
}

func TestEvaluator_Functions(t *testing.T) {
	eval := Evaluator{Resolve: func(q, n string) int { return 0 }, Funcs: NewFuncRegistry()}
	v, err := eval.Eval(&sql.FuncCall{Name: "LOWER", Args: []sql.Expr{&sql.LitString{V: "ABC"}}}, Row{})
	if err != nil {
		t.Fatal(err)
	}
	if v.AsString() != "abc" {
		t.Errorf("LOWER = %q, want abc", v.AsString())
	}
	v, err = eval.Eval(&sql.FuncCall{Name: "COALESCE", Args: []sql.Expr{
		&sql.LitNull{}, &sql.LitString{V: "y"},
	}}, Row{})
	if err != nil {
		t.Fatal(err)
	}
	if v.AsString() != "y" {
		t.Errorf("COALESCE = %q, want y", v.AsString())
	}
}

func TestLikeMatch(t *testing.T) {
	cases := []struct {
		s, pat string
		fold   bool
		want   bool
	}{
		{"abc", "a%", false, true},
		{"abc", "%c", false, true},
		{"abc", "a_c", false, true},
		{"abc", "a__", false, true},
		{"abc", "b%", false, false},
		{"ABC", "abc", false, false}, // LIKE is case-sensitive
		{"ABC", "abc", true, true},   // ILIKE folds case
		{"abc", "ABC", true, true},
		{"ABC", "a%", true, true},
	}
	for _, c := range cases {
		if got := likeMatch(c.s, c.pat, c.fold); got != c.want {
			t.Errorf("likeMatch(%q,%q,fold=%v)=%v, want %v", c.s, c.pat, c.fold, got, c.want)
		}
	}
}

func TestEvalCase(t *testing.T) {
	schema := Schema{Columns: []Column{{Name: "n", Type: TypeInt}}}
	eval := Evaluator{Resolve: SchemaResolver(schema, ""), Funcs: NewFuncRegistry()}
	cases := []struct {
		expr sql.Expr
		row  Row
		want Value
	}{
		{
			expr: &sql.CaseExpr{Whens: []sql.CaseWhen{
				{Cond: &sql.BinaryOp{Op: ">", Left: &sql.ColRef{Name: "n"}, Right: &sql.LitInt{V: 5}}, Then: &sql.LitString{V: "big"}},
			}, Else: &sql.LitString{V: "small"}},
			row:  Row{Values: []Value{IntVal(7)}},
			want: StringVal("big"),
		},
		{
			expr: &sql.CaseExpr{Whens: []sql.CaseWhen{
				{Cond: &sql.BinaryOp{Op: ">", Left: &sql.ColRef{Name: "n"}, Right: &sql.LitInt{V: 5}}, Then: &sql.LitString{V: "big"}},
			}, Else: &sql.LitString{V: "small"}},
			row:  Row{Values: []Value{IntVal(3)}},
			want: StringVal("small"),
		},
		{
			expr: &sql.CaseExpr{Whens: []sql.CaseWhen{
				{Cond: &sql.BinaryOp{Op: ">", Left: &sql.ColRef{Name: "n"}, Right: &sql.LitInt{V: 5}}, Then: &sql.LitString{V: "big"}},
			}}, // no ELSE
			row:  Row{Values: []Value{IntVal(3)}},
			want: Null(),
		},
	}
	for _, c := range cases {
		got, err := eval.Eval(c.expr, c.row)
		if err != nil {
			t.Fatalf("Eval error: %v", err)
		}
		if got != c.want {
			t.Errorf("Eval(Case) = %v, want %v", got, c.want)
		}
	}
}

func TestEvalCast(t *testing.T) {
	cases := []struct {
		v    Value
		typ  string
		want Value
	}{
		{IntVal(42), "float", FloatVal(42)},
		{FloatVal(3.9), "int", IntVal(3)},
		{IntVal(7), "string", StringVal("7")},
		{StringVal("true"), "bool", BoolVal(true)},
		{Null(), "int", Null()},
	}
	for _, c := range cases {
		got, err := Cast(c.v, c.typ)
		if err != nil {
			t.Fatalf("Cast(%v, %s) error: %v", c.v, c.typ, err)
		}
		if got != c.want {
			t.Errorf("Cast(%v, %s) = %v, want %v", c.v, c.typ, got, c.want)
		}
	}
}

func TestCastStrict(t *testing.T) {
	// Lenient mode: bad coercion yields NULL.
	got, err := castWithMode(StringVal("notanumber"), "int", false)
	if err != nil {
		t.Fatalf("lenient cast error: %v", err)
	}
	if !got.IsNull() {
		t.Errorf("lenient cast = %v, want NULL", got)
	}
	// Strict mode: bad coercion yields an error.
	_, err = castWithMode(StringVal("notanumber"), "int", true)
	if err == nil {
		t.Error("strict cast expected error, got nil")
	}
	// Strict mode: valid coercion still succeeds.
	got, err = castWithMode(IntVal(42), "float", true)
	if err != nil {
		t.Fatalf("strict valid cast error: %v", err)
	}
	if got != FloatVal(42) {
		t.Errorf("strict cast = %v, want 42", got)
	}
	// Strict mode: NULL stays NULL (not an error).
	got, err = castWithMode(Null(), "int", true)
	if err != nil {
		t.Fatalf("strict NULL cast error: %v", err)
	}
	if !got.IsNull() {
		t.Errorf("strict NULL cast = %v, want NULL", got)
	}
}

func TestEvalExtract(t *testing.T) {
	tm, _ := parseTime("2024-03-15T14:30:00Z")
	cases := []struct {
		field string
		src   Value
		want  Value
	}{
		{"YEAR", TimeVal(tm), IntVal(2024)},
		{"MONTH", TimeVal(tm), IntVal(3)},
		{"DAY", TimeVal(tm), IntVal(15)},
		{"HOUR", TimeVal(tm), IntVal(14)},
		{"MINUTE", TimeVal(tm), IntVal(30)},
		{"DOW", TimeVal(tm), IntVal(5)}, // Friday
		{"DOY", TimeVal(tm), IntVal(75)},
		{"YEAR", StringVal("2024-03-15"), IntVal(2024)},
		{"YEAR", Null(), Null()},
	}
	for _, c := range cases {
		got, err := extractField(c.src, c.field)
		if err != nil {
			t.Fatalf("extractField(%s) error: %v", c.field, err)
		}
		if got != c.want {
			t.Errorf("extractField(%s) = %v, want %v", c.field, got, c.want)
		}
	}
}

func TestNewStringFunctions(t *testing.T) {
	fr := NewFuncRegistry()
	cases := []struct {
		name string
		args []Value
		want Value
	}{
		{"LEFT", []Value{StringVal("hello"), IntVal(3)}, StringVal("hel")},
		{"RIGHT", []Value{StringVal("hello"), IntVal(3)}, StringVal("llo")},
		{"STRPOS", []Value{StringVal("hello"), StringVal("ll")}, IntVal(3)},
		{"SPLIT_PART", []Value{StringVal("a,b,c"), StringVal(","), IntVal(2)}, StringVal("b")},
		{"REVERSE", []Value{StringVal("abc")}, StringVal("cba")},
		{"REPEAT", []Value{StringVal("ab"), IntVal(3)}, StringVal("ababab")},
		{"INITCAP", []Value{StringVal("hello world")}, StringVal("Hello World")},
		{"LPAD", []Value{StringVal("5"), IntVal(3), StringVal("0")}, StringVal("005")},
		{"RPAD", []Value{StringVal("5"), IntVal(3), StringVal("-")}, StringVal("5--")},
		{"REGEXP_REPLACE", []Value{StringVal("phone: 555"), StringVal("[0-9]+"), StringVal("X")}, StringVal("phone: X")},
		{"LEFT", []Value{Null(), IntVal(3)}, Null()},
	}
	for _, c := range cases {
		fn := fr.Lookup(c.name)
		if fn == nil {
			t.Errorf("function %s not registered", c.name)
			continue
		}
		got, err := fn(c.args)
		if err != nil {
			t.Errorf("%s(%v) error: %v", c.name, c.args, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s(%v) = %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

func TestNewTimeFunctions(t *testing.T) {
	fr := NewFuncRegistry()
	tm, _ := parseTime("2024-03-15T14:30:00Z")

	trunc, err := fr.Lookup("DATE_TRUNC")([]Value{StringVal("month"), TimeVal(tm)})
	if err != nil {
		t.Fatalf("DATE_TRUNC error: %v", err)
	}
	want, _ := parseTime("2024-03-01T00:00:00Z")
	if !trunc.V.(time.Time).Equal(want) {
		t.Errorf("DATE_TRUNC = %v, want %v", trunc, want)
	}

	add, err := fr.Lookup("DATE_ADD")([]Value{TimeVal(tm), StringVal("1 day")})
	if err != nil {
		t.Fatalf("DATE_ADD error: %v", err)
	}
	wantAdd, _ := parseTime("2024-03-16T14:30:00Z")
	if !add.V.(time.Time).Equal(wantAdd) {
		t.Errorf("DATE_ADD = %v, want %v", add, wantAdd)
	}

	ts, err := fr.Lookup("TO_TIMESTAMP")([]Value{IntVal(0)})
	if err != nil {
		t.Fatalf("TO_TIMESTAMP error: %v", err)
	}
	if !ts.V.(time.Time).Equal(time.Unix(0, 0)) {
		t.Errorf("TO_TIMESTAMP(0) = %v", ts)
	}

	sf, err := fr.Lookup("STRFTIME")([]Value{StringVal("%Y"), TimeVal(tm)})
	if err != nil {
		t.Fatalf("STRFTIME error: %v", err)
	}
	if sf.AsString() != "2024" {
		t.Errorf("STRFTIME = %q, want 2024", sf.AsString())
	}
}