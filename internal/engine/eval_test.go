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

func TestNullif(t *testing.T) {
	fr := NewFuncRegistry()
	cases := []struct {
		a, b Value
		want Value
	}{
		{IntVal(5), IntVal(5), Null()},    // equal -> NULL
		{IntVal(5), IntVal(3), IntVal(5)}, // unequal -> a
		{StringVal("x"), StringVal("x"), Null()},
		{IntVal(5), FloatVal(5), Null()}, // cross-type numeric equality
		{Null(), IntVal(1), Null()},      // a NULL -> a (NULL)
		{IntVal(1), Null(), IntVal(1)},   // b NULL -> a
	}
	for _, c := range cases {
		got, err := fr.Lookup("NULLIF")([]Value{c.a, c.b})
		if err != nil {
			t.Fatalf("NULLIF(%v,%v): %v", c.a, c.b, err)
		}
		if got != c.want {
			t.Errorf("NULLIF(%v,%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if _, err := fr.Lookup("NULLIF")([]Value{IntVal(1)}); err == nil {
		t.Error("NULLIF with 1 arg: expected an error")
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
		// REGEXP_EXTRACT: default first group, explicit group, whole match, misses.
		{"REGEXP_EXTRACT", []Value{StringVal("status: 200 len: 5"), StringVal(`status: (\d+)`)}, StringVal("200")},
		{"REGEXP_EXTRACT", []Value{StringVal("status: 200"), StringVal(`status: (\d+)`), IntVal(1)}, StringVal("200")},
		{"REGEXP_EXTRACT", []Value{StringVal("status: 200"), StringVal(`status: (\d+)`), IntVal(0)}, StringVal("status: 200")},
		{"REGEXP_EXTRACT", []Value{StringVal("no match here"), StringVal(`x(\d+)`)}, Null()},
		{"REGEXP_EXTRACT", []Value{StringVal("abc 42"), StringVal(`(\d+)`), IntVal(5)}, Null()}, // group out of range
		{"REGEXP_EXTRACT", []Value{Null(), StringVal(`(\d+)`)}, Null()},
		{"REGEXP_MATCHES", []Value{StringVal("a1b2"), StringVal(`(\d)`)}, StringVal("1")}, // alias still works
		// EXTRACT_VALUE: key: value, key=value, quoted value, miss, boundary.
		{"EXTRACT_VALUE", []Value{StringVal("GET /x status: 200 len: 5"), StringVal("status")}, StringVal("200")},
		{"EXTRACT_VALUE", []Value{StringVal("level=info user=alice"), StringVal("level")}, StringVal("info")},
		{"EXTRACT_VALUE", []Value{StringVal(`level=info note="hello world" n=1`), StringVal("note")}, StringVal("hello world")},
		{"EXTRACT_VALUE", []Value{StringVal("a: 1 b: 2"), StringVal("zzz")}, Null()},               // missing key
		{"EXTRACT_VALUE", []Value{StringVal("xlen: 5"), StringVal("len")}, Null()},                 // key only as a substring
		{"EXTRACT_VALUE", []Value{StringVal("k=1,j=2,status: 9"), StringVal("j")}, StringVal("2")}, // comma-separated
		{"EXTRACT_VALUE", []Value{Null(), StringVal("k")}, Null()},
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

func TestParseTimeNumericOffset(t *testing.T) {
	// Offset without a colon (-0700) and space-separated offset must parse and
	// resolve to the right instant (RFC3339 alone rejects these).
	want, _ := parseTime("2024-03-01T17:15:00Z")
	for _, s := range []string{
		"2024-03-01T10:15:00-0700",
		"2024-03-01 10:15:00-07:00",
		"2024-03-01 10:15:00 -0700",
	} {
		got, err := parseTime(s)
		if err != nil {
			t.Errorf("parseTime(%q) error: %v", s, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, want instant %v", s, got, want)
		}
	}
}

func TestTimezoneFunctions(t *testing.T) {
	fr := NewFuncRegistry()
	utc, _ := parseTime("2024-03-01T17:15:00Z")

	// CONVERT_TZ keeps the instant, changes the displayed zone.
	for _, c := range []struct {
		zone     string
		wantHour int // hour in the target zone
	}{
		{"America/Los_Angeles", 9}, // PST (-08:00) on this date
		{"+05:30", 22},
		{"UTC", 17},
	} {
		got, err := fr.Lookup("CONVERT_TZ")([]Value{TimeVal(utc), StringVal(c.zone)})
		if err != nil {
			t.Fatalf("CONVERT_TZ %s: %v", c.zone, err)
		}
		tv := got.V.(time.Time)
		if !tv.Equal(utc) {
			t.Errorf("CONVERT_TZ %s shifted the instant: %v != %v", c.zone, tv, utc)
		}
		if tv.Hour() != c.wantHour {
			t.Errorf("CONVERT_TZ %s hour = %d, want %d", c.zone, tv.Hour(), c.wantHour)
		}
	}

	// FROM_TZ reinterprets a zone-less (UTC-assumed) wall time as local, shifting
	// the instant. 10:15 as -07:00 is 17:15Z.
	naive, _ := parseTime("2024-03-01 10:15:00")
	got, err := fr.Lookup("FROM_TZ")([]Value{TimeVal(naive), StringVal("-07:00")})
	if err != nil {
		t.Fatalf("FROM_TZ: %v", err)
	}
	if !got.V.(time.Time).Equal(utc) {
		t.Errorf("FROM_TZ(10:15, -07:00) = %v, want instant %v", got, utc)
	}

	// Unknown zone is an error.
	if _, err := fr.Lookup("CONVERT_TZ")([]Value{TimeVal(utc), StringVal("Mars/Olympus")}); err == nil {
		t.Error("CONVERT_TZ with bogus zone: expected error")
	}
}
