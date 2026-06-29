// Package pluginc is the external-plugin connector. A plugin is a standalone
// executable that speaks JSON-RPC 2.0 over its stdio: turntable launches it as
// a subprocess and forwards each Connector interface method (Datasets/Resolve/
// Scan) across the process boundary. This lets users add connectors in any
// language without recompiling turntable — e.g. a program that reports live
// system state as a queryable relation.
//
// The wire protocol is documented in PLUGINS.md. This file defines the wire
// message types and the Content-Length message framing (LSP-style); client.go
// is the RPC client, pluginc.go is the Connector proxy and process manager.
package pluginc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// protocolVersion is the wire protocol version this build speaks. A plugin
// advertises the version it implements in its initialize result; a mismatch is
// reported as an error rather than risking a silent misinterpretation.
const protocolVersion = 1

// ---- JSON-RPC 2.0 envelope ---------------------------------------------------

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcNotification is a request with no id: fire-and-forget, no response (used
// for shutdown).
type rpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

// ---- method params / results -------------------------------------------------

type initializeParams struct {
	ProtocolVersion int            `json:"protocolVersion"`
	Options         map[string]any `json:"options,omitempty"`
}

// capabilities is what a plugin opts into. Every field defaults to false, so a
// plugin that omits the block (or sets nothing) gets the always-correct
// behavior of turntable applying the full predicate/limit/order itself.
type capabilities struct {
	PredicatePushdown  bool `json:"predicatePushdown"`
	ProjectionPushdown bool `json:"projectionPushdown"`
	LimitPushdown      bool `json:"limitPushdown"`
	OrderByPushdown    bool `json:"orderByPushdown"`
}

type initializeResult struct {
	ProtocolVersion int           `json:"protocolVersion"`
	Name            string        `json:"name"`
	Capabilities    capabilities  `json:"capabilities"`
	Datasets        []wireDataset `json:"datasets,omitempty"`
}

type wireDataset struct {
	Name    string         `json:"name"`
	Source  string         `json:"source,omitempty"`
	Options map[string]any `json:"options,omitempty"`
}

type wireColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
}

type wireSchema struct {
	Columns []wireColumn `json:"columns"`
}

type datasetsResult struct {
	Datasets []wireDataset `json:"datasets"`
}

type resolveParams struct {
	Dataset wireDataset `json:"dataset"`
}

type resolveResult struct {
	Schema wireSchema `json:"schema"`
}

type wireOrderTerm struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc"`
}

type scanParams struct {
	Dataset   wireDataset     `json:"dataset"`
	Columns   []string        `json:"columns,omitempty"`
	Limit     *int            `json:"limit,omitempty"`
	Predicate json.RawMessage `json:"predicate,omitempty"`
	OrderBy   []wireOrderTerm `json:"orderBy,omitempty"`
}

// appliedFlags reports which pushdown hints the plugin actually honored. It is
// advisory only: turntable always keeps Filter/Sort/Limit above the Scan, so
// correctness never depends on these. They feed --explain annotation.
type appliedFlags struct {
	Predicate bool `json:"predicate"`
	Limit     bool `json:"limit"`
	OrderBy   bool `json:"orderBy"`
}

type scanResult struct {
	ScanID  string       `json:"scanId"`
	Schema  wireSchema   `json:"schema"`
	Applied appliedFlags `json:"applied"`
}

type nextParams struct {
	ScanID  string `json:"scanId"`
	MaxRows int    `json:"maxRows"`
}

// nextResult carries a batch of rows. Each row is a positional list of raw JSON
// cells aligned to the scan's schema; the cell at index i is decoded against
// column i's declared type (see cell.go).
type nextResult struct {
	Rows [][]json.RawMessage `json:"rows"`
	Done bool                `json:"done"`
}

type closeParams struct {
	ScanID string `json:"scanId"`
}

// ---- message framing (Content-Length, LSP-style) ----------------------------

// writeMessage frames a JSON payload with a Content-Length header and writes it.
func writeMessage(w io.Writer, payload []byte) error {
	if _, err := io.WriteString(w, "Content-Length: "+strconv.Itoa(len(payload))+"\r\n\r\n"); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// readMessage reads one Content-Length-framed JSON payload. Headers other than
// Content-Length are tolerated and ignored (e.g. a Content-Type line).
func readMessage(r *bufio.Reader) ([]byte, error) {
	contentLength := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" { // blank line terminates the header block
			break
		}
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header line %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("bad Content-Length %q", val)
			}
			contentLength = n
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("message missing Content-Length header")
	}
	buf := make([]byte, contentLength)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
