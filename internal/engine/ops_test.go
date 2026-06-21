package engine

import (
	"context"
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
	it := NewHashJoinIter(NewSliceIter(left), NewSliceIter(right), lk, rk, sql.JoinInner, 2)
	got, err := Materialize(context.Background(), it)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("inner join got %d rows, want 2", len(got))
	}
	// Left join: unmatched left row (id=2) should appear with NULLs.
	it2 := NewHashJoinIter(NewSliceIter(left), NewSliceIter(right), lk, rk, sql.JoinLeft, 2)
	got2, err := Materialize(context.Background(), it2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 3 {
		t.Fatalf("left join got %d rows, want 3", len(got2))
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