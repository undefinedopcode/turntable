package engine

import (
	"context"
	"math"
	"testing"

	"github.com/april/turntable/internal/sql"
)

func TestFilterIter(t *testing.T) {
	schema := Schema{Columns: []Column{{Name: "n", Type: TypeInt}}}
	resolver := SchemaResolver(schema, "")
	eval := Evaluator{Resolve: resolver, Funcs: NewFuncRegistry()}
	rows := []Row{
		{Values: []Value{IntVal(1)}},
		{Values: []Value{IntVal(2)}},
		{Values: []Value{IntVal(3)}},
	}
	pred := &sql.BinaryOp{Op: ">", Left: &sql.ColRef{Name: "n"}, Right: &sql.LitInt{V: 1}}
	it := NewFilterIter(NewSliceIter(rows), pred, eval)
	got, err := Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[1].Values[0] != IntVal(3) {
		t.Errorf("second row = %v, want 3", got[1].Values[0])
	}
}

func TestConcatIter(t *testing.T) {
	a := NewSliceIter([]Row{{Values: []Value{IntVal(1)}}, {Values: []Value{IntVal(2)}}})
	b := NewSliceIter([]Row{{Values: []Value{IntVal(3)}}})
	empty := NewSliceIter(nil)
	it := NewConcatIter([]RowIterator{a, empty, b})
	got, err := Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	for i, want := range []int64{1, 2, 3} {
		if v := got[i].Values[0]; v != IntVal(want) {
			t.Errorf("row %d = %v, want %d", i, v, want)
		}
	}
}

func TestConcatIterEmpty(t *testing.T) {
	it := NewConcatIter(nil)
	got, err := Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows, want 0", len(got))
	}
}

func TestSortIter(t *testing.T) {
	schema := Schema{Columns: []Column{{Name: "n", Type: TypeInt}}}
	eval := Evaluator{Resolve: SchemaResolver(schema, ""), Funcs: NewFuncRegistry()}
	rows := []Row{{Values: []Value{IntVal(3)}}, {Values: []Value{IntVal(1)}}, {Values: []Value{IntVal(2)}}}
	it := NewSortIter(NewSliceIter(rows), []sql.OrderTerm{{Expr: &sql.ColRef{Name: "n"}}}, eval)
	got, err := Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{1, 2, 3}
	for i, w := range want {
		if got[i].Values[0] != IntVal(w) {
			t.Errorf("row %d = %v, want %d", i, got[i].Values[0], w)
		}
	}
}

func TestLimitIter(t *testing.T) {
	rows := []Row{{Values: []Value{IntVal(1)}}, {Values: []Value{IntVal(2)}}, {Values: []Value{IntVal(3)}}}
	l := 2
	it := NewLimitIter(NewSliceIter(rows), &l, 1)
	got, err := Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0].Values[0] != IntVal(2) {
		t.Errorf("first = %v, want 2", got[0].Values[0])
	}
}

func TestHashJoinIter(t *testing.T) {
	left := []Row{
		{Values: []Value{IntVal(1), StringVal("a")}},
		{Values: []Value{IntVal(2), StringVal("b")}},
	}
	right := []Row{
		{Values: []Value{IntVal(1), StringVal("x")}},
		{Values: []Value{IntVal(1), StringVal("y")}},
		{Values: []Value{IntVal(9), StringVal("z")}},
	}
	lk := func(r Row) Value { return r.Values[0] }
	rk := func(r Row) Value { return r.Values[0] }
	// left = {(1,a),(2,b)}, right = {(1,x),(1,y),(9,z)}: id=1 yields 2 matches,
	// left id=2 and right id=9 are unmatched.
	it := NewHashJoinIter(NewSliceIter(left), NewSliceIter(right), lk, rk, sql.JoinInner, 2, 2)
	got, err := Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("inner join got %d rows, want 2", len(got))
	}
	// Left join: + unmatched left (id=2), NULL-padded right.
	it2 := NewHashJoinIter(NewSliceIter(left), NewSliceIter(right), lk, rk, sql.JoinLeft, 2, 2)
	got2, err := Materialize(context.Background(), it2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 3 {
		t.Fatalf("left join got %d rows, want 3", len(got2))
	}
	// Right join: 2 matches + unmatched right (id=9), NULL-padded left.
	it3 := NewHashJoinIter(NewSliceIter(left), NewSliceIter(right), lk, rk, sql.JoinRight, 2, 2)
	got3, err := Materialize(context.Background(), it3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got3) != 3 {
		t.Fatalf("right join got %d rows, want 3", len(got3))
	}
	// The unmatched right row (9,z) must have NULL left columns and z on the right.
	var sawNullLeft bool
	for _, r := range got3 {
		if r.Values[0].IsNull() && r.Values[1].IsNull() && r.Values[3].AsString() == "z" {
			sawNullLeft = true
		}
	}
	if !sawNullLeft {
		t.Error("right join: expected an unmatched right row with NULL-padded left")
	}
	// Full join: 2 matches + unmatched left (id=2) + unmatched right (id=9) = 4.
	it4 := NewHashJoinIter(NewSliceIter(left), NewSliceIter(right), lk, rk, sql.JoinFull, 2, 2)
	got4, err := Materialize(context.Background(), it4)
	if err != nil {
		t.Fatal(err)
	}
	if len(got4) != 4 {
		t.Fatalf("full join got %d rows, want 4", len(got4))
	}
}

func TestAggregateIter(t *testing.T) {
	schema := Schema{Columns: []Column{{Name: "grp", Type: TypeString}, {Name: "v", Type: TypeInt}}}
	eval := Evaluator{Resolve: SchemaResolver(schema, ""), Funcs: NewFuncRegistry()}
	rows := []Row{
		{Values: []Value{StringVal("a"), IntVal(1)}},
		{Values: []Value{StringVal("a"), IntVal(3)}},
		{Values: []Value{StringVal("b"), IntVal(5)}},
	}
	outSchema := Schema{Columns: []Column{
		{Name: "grp", Type: TypeString},
		{Name: "count", Type: TypeInt},
		{Name: "sum", Type: TypeFloat},
	}}
	aggs := []AggSpec{
		{Func: "COUNT", Arg: &sql.ColRef{Name: "v"}, Name: "count"},
		{Func: "SUM", Arg: &sql.ColRef{Name: "v"}, Name: "sum"},
	}
	keys := []sql.Expr{&sql.ColRef{Name: "grp"}}
	it := NewAggregateIter(NewSliceIter(rows), keys, aggs, nil, eval, Evaluator{}, outSchema)
	got, err := Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2", len(got))
	}
	// group "a": count=2, sum=4
	if got[0].Values[1] != IntVal(2) {
		t.Errorf("group a count = %v, want 2", got[0].Values[1])
	}
	if got[0].Values[2] != FloatVal(4) {
		t.Errorf("group a sum = %v, want 4", got[0].Values[2])
	}
}

func TestComputeAggExtras(t *testing.T) {
	schema := Schema{Columns: []Column{{Name: "v", Type: TypeInt}, {Name: "s", Type: TypeString}}}
	eval := Evaluator{Resolve: SchemaResolver(schema, ""), Funcs: NewFuncRegistry()}
	mk := func(v int64, s string) Row { return Row{Values: []Value{IntVal(v), StringVal(s)}} }
	// values 2,4,4,6 -> mean 4; sample var = ((4+0+0+4)/3)=2.666..; pop var = 2.
	rows := []Row{mk(2, "a"), mk(4, "b"), mk(4, "b"), mk(6, "c")}
	vcol := &sql.ColRef{Name: "v"}
	scol := &sql.ColRef{Name: "s"}

	cases := []struct {
		spec AggSpec
		want Value
	}{
		{AggSpec{Func: "MEDIAN", Arg: vcol}, FloatVal(4)},                 // (4+4)/2
		{AggSpec{Func: "VAR_POP", Arg: vcol}, FloatVal(2)},                // ((−2)²+0+0+2²)/4
		{AggSpec{Func: "STDDEV_POP", Arg: vcol}, FloatVal(math.Sqrt(2))},  // sqrt(var_pop)
		{AggSpec{Func: "VAR_SAMP", Arg: vcol}, FloatVal(8.0 / 3.0)},       // /(n-1)=3
		{AggSpec{Func: "MEDIAN", Arg: vcol, Distinct: true}, FloatVal(4)}, // {2,4,6} -> 4
		{AggSpec{Func: "STRING_AGG", Arg: scol, Arg2: &sql.LitString{V: ","}}, StringVal("a,b,b,c")},
		{AggSpec{Func: "STRING_AGG", Arg: scol, Arg2: &sql.LitString{V: "-"}, Distinct: true}, StringVal("a-b-c")},
	}
	for _, c := range cases {
		got, err := computeAgg(c.spec, rows, eval)
		if err != nil {
			t.Errorf("%s: %v", c.spec.Func, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s (distinct=%v) = %v, want %v", c.spec.Func, c.spec.Distinct, got, c.want)
		}
	}

	// Sample stddev/variance of a single value is NULL (n-1 == 0).
	one := []Row{mk(5, "x")}
	for _, f := range []string{"STDDEV", "STDDEV_SAMP", "VARIANCE", "VAR_SAMP"} {
		got, err := computeAgg(AggSpec{Func: f, Arg: vcol}, one, eval)
		if err != nil {
			t.Fatal(err)
		}
		if !got.IsNull() {
			t.Errorf("%s of one value = %v, want NULL", f, got)
		}
	}
	// Population forms of a single value are defined (variance 0).
	got, _ := computeAgg(AggSpec{Func: "VAR_POP", Arg: vcol}, one, eval)
	if got != FloatVal(0) {
		t.Errorf("VAR_POP of one value = %v, want 0", got)
	}
}

func TestDistinctIter(t *testing.T) {
	rows := []Row{
		{Values: []Value{IntVal(1)}},
		{Values: []Value{IntVal(1)}},
		{Values: []Value{IntVal(2)}},
	}
	it := NewDistinctIter(NewSliceIter(rows))
	got, err := Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
}