package pluginc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/april/turntable/internal/engine"
)

func TestFramingRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payloads := [][]byte{[]byte(`{"a":1}`), []byte(`{}`), []byte(`{"big":"` + string(make([]byte, 5000)) + `"}`)}
	for _, p := range payloads {
		if err := writeMessage(&buf, p); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	r := bufio.NewReader(&buf)
	for i, want := range payloads {
		got, err := readMessage(r)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("message %d mismatch", i)
		}
	}
}

// fakePlugin serves an in-process plugin over pipes for client/iterator tests.
// It exposes one dataset "nums" with a single int column n = 0..count-1.
type fakePlugin struct {
	count int
	scans map[string][][]any
	next  int
}

func (f *fakePlugin) run(reqR io.Reader, respW io.Writer) {
	if f.scans == nil {
		f.scans = map[string][][]any{}
	}
	br := bufio.NewReader(reqR)
	for {
		payload, err := readMessage(br)
		if err != nil {
			return
		}
		var req struct {
			ID     uint64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(payload, &req)
		if req.Method == "shutdown" {
			return
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
		result, rerr := f.handle(req.Method, req.Params)
		if rerr != nil {
			resp.Error = &rpcError{Code: -32000, Message: rerr.Error()}
		} else {
			resp.Result, _ = json.Marshal(result)
		}
		b, _ := json.Marshal(resp)
		_ = writeMessage(respW, b)
	}
}

func (f *fakePlugin) handle(method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return initializeResult{
			ProtocolVersion: protocolVersion,
			Name:            "fake",
			Datasets:        []wireDataset{{Name: "nums"}},
		}, nil
	case "datasets":
		return datasetsResult{Datasets: []wireDataset{{Name: "nums"}}}, nil
	case "resolve":
		return resolveResult{Schema: numsSchema()}, nil
	case "scan":
		rows := make([][]any, f.count)
		for i := range rows {
			rows[i] = []any{i}
		}
		f.next++
		id := "s" + string(rune('0'+f.next))
		f.scans[id] = rows
		return scanResult{ScanID: id, Schema: numsSchema()}, nil
	case "next":
		var p nextParams
		_ = json.Unmarshal(params, &p)
		rows := f.scans[p.ScanID]
		max := p.MaxRows
		if max > len(rows) {
			max = len(rows)
		}
		batch := rows[:max]
		f.scans[p.ScanID] = rows[max:]
		out := make([][]json.RawMessage, len(batch))
		for i, row := range batch {
			cells := make([]json.RawMessage, len(row))
			for j, c := range row {
				cells[j], _ = json.Marshal(c)
			}
			out[i] = cells
		}
		return nextResult{Rows: out, Done: len(f.scans[p.ScanID]) == 0}, nil
	case "close":
		var p closeParams
		_ = json.Unmarshal(params, &p)
		delete(f.scans, p.ScanID)
		return struct{}{}, nil
	}
	return nil, io.EOF
}

func numsSchema() wireSchema {
	return wireSchema{Columns: []wireColumn{{Name: "n", Type: "int"}}}
}

// pipeClient wires a client to a fakePlugin running in a goroutine.
func pipeClient(t *testing.T, f *fakePlugin) *client {
	t.Helper()
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	go f.run(reqR, respW)
	return newClient(reqW, respR)
}

func TestClientHandshakeAndScan(t *testing.T) {
	f := &fakePlugin{count: 2500} // spans multiple default batches
	cl := pipeClient(t, f)

	var info initializeResult
	if err := cl.call("initialize", initializeParams{ProtocolVersion: protocolVersion}, &info); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if info.Name != "fake" {
		t.Fatalf("name = %q", info.Name)
	}

	var sc scanResult
	if err := cl.call("scan", scanParams{Dataset: wireDataset{Name: "nums"}}, &sc); err != nil {
		t.Fatalf("scan: %v", err)
	}

	it := &scanIter{cl: cl, scanID: sc.ScanID, schema: toSchema(sc.Schema), maxRows: defaultBatchRows}
	defer it.Close()

	var got int
	for {
		row, ok, err := it.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if !ok {
			break
		}
		if row.Values[0].Type != engine.TypeInt || row.Values[0].V.(int64) != int64(got) {
			t.Fatalf("row %d = %+v", got, row.Values[0])
		}
		got++
	}
	if got != f.count {
		t.Fatalf("read %d rows, want %d", got, f.count)
	}
}

func TestClientPropagatesPluginError(t *testing.T) {
	f := &fakePlugin{count: 0}
	cl := pipeClient(t, f)
	// "bogus" is unhandled → fake returns io.EOF as an rpc error.
	err := cl.call("bogus", struct{}{}, nil)
	if err == nil {
		t.Fatal("want error from unhandled method")
	}
}
