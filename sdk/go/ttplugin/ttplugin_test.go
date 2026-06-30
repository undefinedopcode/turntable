package ttplugin

import (
	"bufio"
	"encoding/json"
	"io"
	"testing"
	"time"
)

// testDriver speaks the client side of the protocol to a ServeRW server running
// in a goroutine, so we can exercise the full request/response path.
type testDriver struct {
	w  io.Writer
	r  *bufio.Reader
	id uint64
}

func newDriver(t *testing.T, p Plugin) *testDriver {
	t.Helper()
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	go func() { _ = ServeRW(p, reqR, respW) }()
	return &testDriver{w: reqW, r: bufio.NewReader(respR)}
}

func (d *testDriver) call(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	d.id++
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": d.id, "method": method, "params": params})
	if err := writeMessage(d.w, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := readMessage(d.r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("%s: plugin error: %s", method, resp.Error.Message)
	}
	return resp.Result
}

func samplePlugin() Plugin {
	return Plugin{
		Name: "demo",
		Datasets: map[string]Dataset{
			"items": {
				Schema: Schema{Columns: []Column{
					{Name: "id", Type: "int"},
					{Name: "name", Type: "string"},
					{Name: "ts", Type: "time"},
				}},
				Rows: func(Request) (Rows, error) {
					base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
					return Rows{
						{1, "alpha", base},
						{2, "beta", base.Add(time.Hour)},
						{3, "gamma", base.Add(2 * time.Hour)},
					}, nil
				},
			},
		},
	}
}

func TestServeInitializeAndResolve(t *testing.T) {
	d := newDriver(t, samplePlugin())
	init := d.call(t, "initialize", map[string]any{"protocolVersion": 1})
	if init["name"] != "demo" {
		t.Fatalf("name = %v", init["name"])
	}
	caps := init["capabilities"].(map[string]any)
	if caps["predicatePushdown"] != true || caps["limitPushdown"] != true {
		t.Fatalf("capabilities = %v", caps)
	}
	res := d.call(t, "resolve", map[string]any{"dataset": map[string]string{"name": "items"}})
	sc := res["schema"].(map[string]any)
	cols := sc["columns"].([]any)
	if len(cols) != 3 {
		t.Fatalf("columns = %d", len(cols))
	}
}

func TestServeScanWithPredicateAndLimit(t *testing.T) {
	d := newDriver(t, samplePlugin())

	// WHERE id >= 2  → rows 2,3 ; LIMIT 1 → row 2.
	pred := map[string]any{
		"kind": "compare", "op": ">=", "column": "id",
		"value": map[string]any{"type": "int", "value": 2},
	}
	limit := 1
	sc := d.call(t, "scan", map[string]any{
		"dataset":   map[string]string{"name": "items"},
		"predicate": pred,
		"limit":     limit,
	})
	applied := sc["applied"].(map[string]any)
	if applied["predicate"] != true || applied["limit"] != true {
		t.Fatalf("applied = %v", applied)
	}
	scanID := sc["scanId"].(string)

	next := d.call(t, "next", map[string]any{"scanId": scanID, "maxRows": 100})
	rows := next["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0].([]any)
	if row[0].(float64) != 2 || row[1].(string) != "beta" {
		t.Fatalf("row = %v", row)
	}
	// time cell must be RFC3339-encoded.
	if _, err := time.Parse(time.RFC3339, row[2].(string)); err != nil {
		t.Fatalf("ts not RFC3339: %v (%v)", row[2], err)
	}
	if next["done"] != true {
		t.Fatalf("done = %v", next["done"])
	}
}

func TestServeScanBatching(t *testing.T) {
	p := Plugin{Name: "n", Datasets: map[string]Dataset{
		"nums": {
			Schema: Schema{Columns: []Column{{Name: "n", Type: "int"}}},
			Rows: func(Request) (Rows, error) {
				rows := make(Rows, 2500)
				for i := range rows {
					rows[i] = Row{i}
				}
				return rows, nil
			},
		},
	}}
	d := newDriver(t, p)
	sc := d.call(t, "scan", map[string]any{"dataset": map[string]string{"name": "nums"}})
	id := sc["scanId"].(string)
	total := 0
	for {
		next := d.call(t, "next", map[string]any{"scanId": id, "maxRows": 1000})
		total += len(next["rows"].([]any))
		if next["done"] == true {
			break
		}
	}
	if total != 2500 {
		t.Fatalf("read %d rows, want 2500", total)
	}
}

func TestPredicateEval(t *testing.T) {
	cols := map[string]int{"a": 0, "b": 1, "name": 2}
	row := []any{int64(5), nil, "hello"}
	get := func(c string) any {
		if i, ok := cols[c]; ok {
			return row[i]
		}
		return nil
	}
	lit := func(typ string, v any) *Literal { return &Literal{Type: typ, Value: v} }

	tests := []struct {
		name string
		pred Predicate
		want bool
	}{
		{"compare gt true", Predicate{Kind: "compare", Op: ">", Column: "a", Value: lit("int", 3.0)}, true},
		{"compare gt false", Predicate{Kind: "compare", Op: ">", Column: "a", Value: lit("int", 9.0)}, false},
		{"isnull true", Predicate{Kind: "isnull", Column: "b"}, true},
		{"isnull negate", Predicate{Kind: "isnull", Column: "b", Negate: true}, false},
		{"in true", Predicate{Kind: "in", Column: "a", Values: []Literal{{Type: "int", Value: 1.0}, {Type: "int", Value: 5.0}}}, true},
		{"between true", Predicate{Kind: "between", Column: "a", Low: lit("int", 1.0), High: lit("int", 10.0)}, true},
		{"like true", Predicate{Kind: "like", Column: "name", Pattern: "he%"}, true},
		{"like false", Predicate{Kind: "like", Column: "name", Pattern: "xy%"}, false},
		{"and", Predicate{Kind: "and", Args: []Predicate{
			{Kind: "compare", Op: "=", Column: "a", Value: lit("int", 5.0)},
			{Kind: "like", Column: "name", Pattern: "%llo"},
		}}, true},
		{"or short-circuits past false", Predicate{Kind: "or", Args: []Predicate{
			{Kind: "compare", Op: "=", Column: "a", Value: lit("int", 99.0)},
			{Kind: "isnull", Column: "b"},
		}}, true},
		{"not", Predicate{Kind: "not", Arg: &Predicate{Kind: "isnull", Column: "name"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.pred.Eval(get); got != tt.want {
				t.Errorf("Eval = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestManualPushdownSkipsFiltering(t *testing.T) {
	p := samplePlugin()
	p.ManualPushdown = true
	d := newDriver(t, p)
	pred := map[string]any{"kind": "compare", "op": "=", "column": "id",
		"value": map[string]any{"type": "int", "value": 1}}
	sc := d.call(t, "scan", map[string]any{
		"dataset": map[string]string{"name": "items"}, "predicate": pred})
	applied := sc["applied"].(map[string]any)
	if len(applied) != 0 {
		t.Fatalf("manual mode should report nothing applied, got %v", applied)
	}
	id := sc["scanId"].(string)
	next := d.call(t, "next", map[string]any{"scanId": id, "maxRows": 100})
	// All 3 rows come back unfiltered (turntable would apply the WHERE).
	if got := len(next["rows"].([]any)); got != 3 {
		t.Fatalf("manual mode returned %d rows, want 3 (unfiltered)", got)
	}
}
