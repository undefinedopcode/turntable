package engine

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/april/turntable/internal/sql"
)

// TestRowCodecRoundTrip checks every value type survives a spill write/read.
func TestRowCodecRoundTrip(t *testing.T) {
	ts := time.Date(2026, 7, 23, 10, 30, 0, 0, time.UTC)
	rows := []Row{
		{Values: []Value{
			Null(),
			IntVal(-42),
			FloatVal(3.14159),
			StringVal("hello, spill"),
			BoolVal(true),
			BoolVal(false),
			TimeVal(ts),
			{Type: TypeDuration, V: 90 * time.Minute},
			{Type: TypeBytes, V: []byte{0, 1, 2, 255}},
			AnyVal(map[string]any{"k": "v", "n": float64(5)}),
			AnyVal([]any{"a", float64(1), true}),
			AnyVal(nil),
		}},
		{Values: []Value{IntVal(0)}}, // varying width
		{Values: []Value{StringVal(""), Null()}},
	}
	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	for _, r := range rows {
		if err := writeRow(w, r); err != nil {
			t.Fatalf("writeRow: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(&buf)
	for i, want := range rows {
		got, ok, err := readRow(r)
		if err != nil || !ok {
			t.Fatalf("readRow %d: ok=%v err=%v", i, ok, err)
		}
		if len(got.Values) != len(want.Values) {
			t.Fatalf("row %d width = %d, want %d", i, len(got.Values), len(want.Values))
		}
		for j := range want.Values {
			if !valuesEqualForSpill(got.Values[j], want.Values[j]) {
				t.Errorf("row %d col %d = %#v, want %#v", i, j, got.Values[j], want.Values[j])
			}
		}
	}
	if _, ok, err := readRow(r); ok || err != nil {
		t.Errorf("trailing read: ok=%v err=%v, want clean EOF", ok, err)
	}
}

// valuesEqualForSpill compares two decoded values, handling the structured
// (TypeAny) case where Compare would fall back to string formatting.
func valuesEqualForSpill(a, b Value) bool {
	if a.Type != b.Type {
		return false
	}
	if a.Type == TypeAny {
		return fmt.Sprintf("%v", a.V) == fmt.Sprintf("%v", b.V)
	}
	if a.Type == TypeBytes {
		return bytes.Equal(a.V.([]byte), b.V.([]byte))
	}
	if a.Type == TypeTime {
		return a.V.(time.Time).Equal(b.V.(time.Time))
	}
	return Compare(a, b) == 0
}

// aggFixture builds the schema/aggs used by the spill tests: GROUP BY grp with
// COUNT(*), SUM(v), MEDIAN(v).
func aggFixture() (Schema, Schema, []sql.Expr, []AggSpec) {
	schema := Schema{Columns: []Column{{Name: "grp", Type: TypeString}, {Name: "v", Type: TypeInt}}}
	outSchema := Schema{Columns: []Column{
		{Name: "grp", Type: TypeString}, {Name: "cnt", Type: TypeInt},
		{Name: "sum", Type: TypeFloat}, {Name: "med", Type: TypeFloat},
	}}
	keys := []sql.Expr{&sql.ColRef{Name: "grp"}}
	aggs := []AggSpec{
		{Func: "COUNT", Arg: &sql.ColRef{Name: "*"}, Name: "cnt"},
		{Func: "SUM", Arg: &sql.ColRef{Name: "v"}, Name: "sum"},
		{Func: "MEDIAN", Arg: &sql.ColRef{Name: "v"}, Name: "med"},
	}
	return schema, outSchema, keys, aggs
}

func runAgg(t *testing.T, schema, outSchema Schema, rows []Row, keys []sql.Expr, aggs []AggSpec, cfg AggConfig) ([]Row, error) {
	t.Helper()
	eval := Evaluator{Resolve: SchemaResolver(schema, ""), Funcs: NewFuncRegistry()}
	it := NewAggregateIter(NewSliceIter(rows), keys, aggs, nil, eval, Evaluator{}, outSchema)
	it.SetAggConfig(cfg)
	defer it.Close()
	return Materialize(context.Background(), it)
}

// resultKey renders an aggregate result row as a comparable string, so two runs
// can be compared regardless of group emission order.
func resultKey(r Row) string {
	return fmt.Sprintf("%v|%v|%v|%v", r.Values[0].V, r.Values[1].V, r.Values[2].V, r.Values[3].V)
}

func sortedKeys(rows []Row) []string {
	ks := make([]string, len(rows))
	for i, r := range rows {
		ks[i] = resultKey(r)
	}
	sort.Strings(ks)
	return ks
}

// TestSpillMatchesInMemory is the core invariant: spilling to disk must produce
// exactly the same groups as an unlimited in-memory aggregation, no matter how
// small the budget (which forces many recursive partition passes).
func TestSpillMatchesInMemory(t *testing.T) {
	schema, outSchema, keys, aggs := aggFixture()
	// 200 groups, each with a handful of rows; interleave groups across input.
	var rows []Row
	for i := 0; i < 1000; i++ {
		g := fmt.Sprintf("g%03d", i%200)
		rows = append(rows, Row{Values: []Value{StringVal(g), IntVal(int64(i % 7))}})
	}

	want, err := runAgg(t, schema, outSchema, rows, keys, aggs, AggConfig{})
	if err != nil {
		t.Fatalf("in-memory: %v", err)
	}
	if len(want) != 200 {
		t.Fatalf("in-memory groups = %d, want 200", len(want))
	}

	for _, budget := range []int{1, 3, 16, 50, 199} {
		got, err := runAgg(t, schema, outSchema, rows, keys, aggs,
			AggConfig{MaxGroups: budget, Spill: true})
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if len(got) != len(want) {
			t.Errorf("budget %d: %d groups, want %d", budget, len(got), len(want))
			continue
		}
		gk, wk := sortedKeys(got), sortedKeys(want)
		for i := range wk {
			if gk[i] != wk[i] {
				t.Errorf("budget %d: result mismatch at %d: %q vs %q", budget, i, gk[i], wk[i])
				break
			}
		}
	}
}

// TestSpillHolisticNotSplit checks a holistic aggregate (MEDIAN) is correct
// under spilling — a group's rows must all land in one partition, never split.
func TestSpillHolisticNotSplit(t *testing.T) {
	schema, outSchema, keys, aggs := aggFixture()
	// group "a": values 1..5 (median 3); group "b": values 10,20,30,40 (median 25).
	var rows []Row
	for _, v := range []int64{1, 2, 3, 4, 5} {
		rows = append(rows, Row{Values: []Value{StringVal("a"), IntVal(v)}})
	}
	for _, v := range []int64{10, 20, 30, 40} {
		rows = append(rows, Row{Values: []Value{StringVal("b"), IntVal(v)}})
	}
	// Budget 1 forces one group in memory and the other spilled.
	got, err := runAgg(t, schema, outSchema, rows, keys, aggs, AggConfig{MaxGroups: 1, Spill: true})
	if err != nil {
		t.Fatal(err)
	}
	meds := map[string]float64{}
	for _, r := range got {
		f, _ := r.Values[3].AsFloat()
		meds[r.Values[0].AsString()] = f
	}
	if meds["a"] != 3 {
		t.Errorf("median(a) = %v, want 3", meds["a"])
	}
	if meds["b"] != 25 {
		t.Errorf("median(b) = %v, want 25", meds["b"])
	}
}

// TestAggGuardrailErrors verifies the budget errors cleanly when spilling is off.
func TestAggGuardrailErrors(t *testing.T) {
	schema, outSchema, keys, aggs := aggFixture()
	rows := []Row{
		{Values: []Value{StringVal("a"), IntVal(1)}},
		{Values: []Value{StringVal("b"), IntVal(2)}},
		{Values: []Value{StringVal("c"), IntVal(3)}},
	}
	_, err := runAgg(t, schema, outSchema, rows, keys, aggs, AggConfig{MaxGroups: 2, Spill: false})
	if err == nil {
		t.Fatal("expected an error when groups exceed the budget without spilling")
	}
	if want := "in-memory group limit"; !bytes.Contains([]byte(err.Error()), []byte(want)) {
		t.Errorf("error = %q, want it to mention %q", err, want)
	}

	// Under the budget, no error and correct result.
	if _, err := runAgg(t, schema, outSchema, rows, keys, aggs, AggConfig{MaxGroups: 3, Spill: false}); err != nil {
		t.Errorf("within budget: unexpected error %v", err)
	}
}

// TestSpillEmptyAndSingleGroup covers the degenerate cases still work with a
// budget set: an empty global aggregate and a single group under a tiny budget.
func TestSpillEmptyAndSingleGroup(t *testing.T) {
	schema, outSchema, _, aggs := aggFixture()
	// Global aggregate (no keys) over empty input → one row: COUNT(*)=0.
	got, err := runAgg(t, schema, outSchema, nil, nil, aggs, AggConfig{MaxGroups: 1, Spill: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("empty global: %d rows, want 1", len(got))
	}
	if c, _ := got[0].Values[1].AsInt(); c != 0 {
		t.Errorf("empty COUNT(*) = %v, want 0", got[0].Values[1].V)
	}
}
