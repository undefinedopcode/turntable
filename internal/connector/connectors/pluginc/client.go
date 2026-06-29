package pluginc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// client is a minimal synchronous JSON-RPC 2.0 client over a framed byte
// stream. Calls are serialized by a mutex: at most one request is in flight at
// a time, so the next framed response is always the one we are waiting for.
// This keeps the client simple while still supporting many concurrently *open*
// scans (each identified by its own scanId) — only the individual RPC calls are
// serialized, not the logical cursors.
type client struct {
	mu sync.Mutex
	w  io.Writer
	r  *bufio.Reader
	id uint64
}

func newClient(w io.Writer, r io.Reader) *client {
	return &client{w: w, r: bufio.NewReader(r)}
}

// call sends a request and decodes the result into out (which may be nil to
// discard it). It returns the plugin's error verbatim when the response carries
// one.
func (c *client) call(method string, params, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.id++
	id := c.id
	reqBytes, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("plugin %s: marshal request: %w", method, err)
	}
	if err := writeMessage(c.w, reqBytes); err != nil {
		return fmt.Errorf("plugin %s: write: %w", method, err)
	}

	for {
		raw, err := readMessage(c.r)
		if err != nil {
			return fmt.Errorf("plugin %s: read: %w", method, err)
		}
		var resp rpcResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("plugin %s: decode response: %w", method, err)
		}
		if resp.ID != id {
			// Serialized calls should never see a stale id, but skip defensively
			// rather than misattribute a response.
			continue
		}
		if resp.Error != nil {
			return fmt.Errorf("plugin %s: %s", method, resp.Error.Message)
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("plugin %s: decode result: %w", method, err)
			}
		}
		return nil
	}
}

// notify sends a fire-and-forget notification (no id, no response read).
func (c *client) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := json.Marshal(rpcNotification{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return writeMessage(c.w, b)
}
