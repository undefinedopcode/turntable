package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/connector/connectors/memc"
	"github.com/april/turntable/internal/engine"
)

// A snapshot exposes a client-held result set (rows the browser already has,
// e.g. for a chart) as a queryable FROM target — SELECT * FROM _snapshot_<id>
// WHERE ... — without replanning or re-executing the query that produced them.
// Unlike CREATE MATERIALIZED VIEW it has no defining query to remember and
// upserts rather than erroring on a name clash, since it's disposable UI state
// (one snapshot per chart/tab) rather than a user-named persistent object.

// snapshotNamePrefix marks a registered source as snapshot-owned, so it can be
// filtered out of source listings/autocomplete and distinguished from a
// regular source or materialized view when dropped.
const snapshotNamePrefix = "_snapshot_"

// snapshotIDPattern restricts the caller-supplied id to a safe charset: it
// becomes part of a name registered in the process-global connector registry,
// so it must not collide with or spoof an unrelated source.
var snapshotIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type snapshotRequest struct {
	ID      string      `json:"id"`
	Columns []apiColumn `json:"columns"`
	Rows    [][]any     `json:"rows"`
}

// handleSnapshot creates/replaces a snapshot (POST) or drops one (DELETE).
func (a *App) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		a.createSnapshot(w, r)
	case http.MethodDelete:
		a.deleteSnapshot(w, r)
	default:
		http.Error(w, "POST or DELETE", http.StatusMethodNotAllowed)
	}
}

// createSnapshot registers req.Rows/Columns as an in-memory source named
// "_snapshot_<id>", replacing any existing snapshot under the same id.
func (a *App) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req snapshotRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !snapshotIDPattern.MatchString(req.ID) {
		writeJSON(w, map[string]any{"error": "id must be 1-64 chars of letters, digits, '-', '_'"})
		return
	}
	schema, rows, err := schemaRowsFromWire(req.Columns, req.Rows)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}

	name := snapshotNamePrefix + req.ID
	// Drop any prior snapshot under this id first so RegisterSource's
	// already-registered check doesn't reject the replacement.
	a.mem.Drop(name)
	a.Reg.RemoveSource(name)
	a.mem.Put(name, &memc.Table{Schema: schema, Rows: rows, Populated: true})
	if err := a.Reg.RegisterSource(name, a.mem, connector.Dataset{Name: name, Source: name}); err != nil {
		a.mem.Drop(name)
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"name": name, "rows": len(rows)})
}

// deleteSnapshot drops a previously-registered snapshot (e.g. when its owning
// tab closes). Dropping an unknown id is a no-op, not an error.
func (a *App) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if !snapshotIDPattern.MatchString(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	name := snapshotNamePrefix + id
	a.mem.Drop(name)
	a.Reg.RemoveSource(name)
	writeJSON(w, map[string]any{"dropped": name})
}

// schemaRowsFromWire converts the JSON-decoded columns/rows of a /api/query
// response back into an engine.Schema/[]engine.Row pair — the inverse of
// jsonValue. Rows is [][]any as produced by encoding/json, so numbers decode
// to float64 regardless of declared column type.
func schemaRowsFromWire(cols []apiColumn, raw [][]any) (engine.Schema, []engine.Row, error) {
	schema := engine.Schema{Columns: make([]engine.Column, len(cols))}
	types := make([]engine.Type, len(cols))
	for i, c := range cols {
		t, ok := parseAPIType(c.Type)
		if !ok {
			return engine.Schema{}, nil, fmt.Errorf("snapshot: unknown column type %q for %q", c.Type, c.Name)
		}
		types[i] = t
		schema.Columns[i] = engine.Column{Name: c.Name, Type: t, Nullable: c.Nullable}
	}
	rows := make([]engine.Row, len(raw))
	for ri, r := range raw {
		if len(r) != len(cols) {
			return engine.Schema{}, nil, fmt.Errorf("snapshot: row %d has %d cells, want %d", ri, len(r), len(cols))
		}
		vals := make([]engine.Value, len(cols))
		for ci, cell := range r {
			v, err := cellToValue(cell, types[ci])
			if err != nil {
				return engine.Schema{}, nil, fmt.Errorf("snapshot: column %q: %w", cols[ci].Name, err)
			}
			vals[ci] = v
		}
		rows[ri] = engine.Row{Values: vals}
	}
	return schema, rows, nil
}

// cellToValue converts one JSON-decoded cell back into an engine.Value typed
// per the declared column type. A raw nil is always NULL, independent of the
// declared type, mirroring jsonValue's NULL -> nil direction.
func cellToValue(raw any, t engine.Type) (engine.Value, error) {
	if raw == nil {
		return engine.Null(), nil
	}
	switch t {
	case engine.TypeInt:
		f, ok := raw.(float64)
		if !ok {
			return engine.Value{}, fmt.Errorf("want a number, got %T", raw)
		}
		return engine.IntVal(int64(f)), nil
	case engine.TypeFloat:
		f, ok := raw.(float64)
		if !ok {
			return engine.Value{}, fmt.Errorf("want a number, got %T", raw)
		}
		return engine.FloatVal(f), nil
	case engine.TypeBool:
		b, ok := raw.(bool)
		if !ok {
			return engine.Value{}, fmt.Errorf("want a bool, got %T", raw)
		}
		return engine.BoolVal(b), nil
	case engine.TypeString:
		s, ok := raw.(string)
		if !ok {
			return engine.Value{}, fmt.Errorf("want a string, got %T", raw)
		}
		return engine.StringVal(s), nil
	case engine.TypeTime:
		s, ok := raw.(string)
		if !ok {
			return engine.Value{}, fmt.Errorf("want an RFC3339 string, got %T", raw)
		}
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return engine.Value{}, fmt.Errorf("bad time %q: %w", s, err)
		}
		return engine.TimeVal(tm), nil
	case engine.TypeDuration:
		// jsonValue has no special case for TypeDuration, so it serializes as
		// the plain JSON number of nanoseconds (time.Duration's underlying int64).
		f, ok := raw.(float64)
		if !ok {
			return engine.Value{}, fmt.Errorf("want a number (nanoseconds), got %T", raw)
		}
		return engine.Value{Type: engine.TypeDuration, V: time.Duration(int64(f))}, nil
	case engine.TypeBytes:
		// encoding/json base64-encodes a []byte automatically; decode the same way.
		s, ok := raw.(string)
		if !ok {
			return engine.Value{}, fmt.Errorf("want a base64 string, got %T", raw)
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return engine.Value{}, fmt.Errorf("bad base64: %w", err)
		}
		return engine.Value{Type: engine.TypeBytes, V: b}, nil
	default:
		// TypeAny (and any other/unknown declared type): pass the decoded JSON
		// value through as-is.
		return engine.AnyVal(raw), nil
	}
}

// parseAPIType reverses engine.Type.String() (the value apiColumn.Type holds).
func parseAPIType(s string) (engine.Type, bool) {
	switch s {
	case "null":
		return engine.TypeNull, true
	case "int":
		return engine.TypeInt, true
	case "float":
		return engine.TypeFloat, true
	case "string":
		return engine.TypeString, true
	case "bool":
		return engine.TypeBool, true
	case "time":
		return engine.TypeTime, true
	case "duration":
		return engine.TypeDuration, true
	case "bytes":
		return engine.TypeBytes, true
	case "any":
		return engine.TypeAny, true
	default:
		return engine.TypeInvalid, false
	}
}
