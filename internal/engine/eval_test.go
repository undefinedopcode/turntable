package engine

import (
	"testing"

	"github.com/april/octoparser/internal/sql"
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
		want   bool
	}{
		{"abc", "a%", true},
		{"abc", "%c", true},
		{"abc", "a_c", true},
		{"abc", "a__", true},
		{"abc", "b%", false},
		{"ABC", "abc", true}, // case-insensitive
	}
	for _, c := range cases {
		if got := likeMatch(c.s, c.pat); got != c.want {
			t.Errorf("likeMatch(%q,%q)=%v, want %v", c.s, c.pat, got, c.want)
		}
	}
}