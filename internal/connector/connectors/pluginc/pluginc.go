package pluginc

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/april/turntable/internal/connector"
	"github.com/april/turntable/internal/engine"
)

// defaultBatchRows is how many rows turntable asks for per `next` call. Batching
// bounds memory and gives the connector backpressure (it stops pulling once a
// LIMIT is satisfied upstream and the iterator is Closed).
const defaultBatchRows = 1000

// Connector proxies the connector.Connector interface to an external plugin
// process over JSON-RPC. One Connector owns one long-lived subprocess; multiple
// datasets exposed by that plugin share the process. It is started lazily on
// first use (or eagerly at registration via Start) and torn down by Close.
type Connector struct {
	command []string
	opts    map[string]any
	stderr  io.Writer // where the plugin's stderr is forwarded (default os.Stderr)

	mu      sync.Mutex
	started bool
	cmd     *exec.Cmd
	cl      *client
	info    initializeResult
	stdin   io.Closer
}

// New constructs a plugin connector that will run the given command. command[0]
// is the executable; the rest are its arguments. opts are passed to the plugin
// verbatim in the initialize handshake.
func New(command []string, opts map[string]any) *Connector {
	return &Connector{command: command, opts: opts, stderr: os.Stderr}
}

// Name returns the prefix the plugin advertised in its handshake. Before the
// plugin has started it returns "plugin"; registration calls Start first, so by
// the time the registry asks, the advertised name is available.
func (c *Connector) Name() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started && c.info.Name != "" {
		return c.info.Name
	}
	return "plugin"
}

// Capabilities reports the pushdown features the plugin opted into (valid after
// Start). Used to decide which hints to send.
func (c *Connector) Capabilities() capabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info.Capabilities
}

// Start spawns the subprocess and performs the initialize handshake. It is
// idempotent and safe to call from registration; subsequent interface calls
// reuse the running process.
func (c *Connector) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensureStarted(ctx)
}

// ensureStarted must be called with c.mu held.
func (c *Connector) ensureStarted(ctx context.Context) error {
	if c.started {
		return nil
	}
	if len(c.command) == 0 {
		return fmt.Errorf("plugin connector: empty command")
	}
	cmd := exec.Command(c.command[0], c.command[1:]...)
	cmd.Stderr = c.stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("plugin %q: stdin: %w", c.command[0], err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("plugin %q: stdout: %w", c.command[0], err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("plugin %q: start: %w", c.command[0], err)
	}
	c.cmd = cmd
	c.stdin = stdin
	c.cl = newClient(stdin, stdout)

	var info initializeResult
	if err := c.cl.call("initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		Options:         c.opts,
	}, &info); err != nil {
		_ = c.stop()
		return fmt.Errorf("plugin %q: %w", c.command[0], err)
	}
	if info.ProtocolVersion != protocolVersion {
		_ = c.stop()
		return fmt.Errorf("plugin %q: protocol version %d unsupported (want %d)",
			c.command[0], info.ProtocolVersion, protocolVersion)
	}
	if info.Name == "" {
		_ = c.stop()
		return fmt.Errorf("plugin %q: handshake did not advertise a name", c.command[0])
	}
	c.info = info
	c.started = true
	return nil
}

// Datasets returns the datasets the plugin exposes. Many plugins advertise them
// in the initialize handshake; if so we reuse that and skip a round-trip,
// otherwise we ask explicitly.
func (c *Connector) Datasets(ctx context.Context) ([]connector.Dataset, error) {
	c.mu.Lock()
	if err := c.ensureStarted(ctx); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	advertised := c.info.Datasets
	cl := c.cl
	c.mu.Unlock()

	wire := advertised
	if len(wire) == 0 {
		var res datasetsResult
		if err := cl.call("datasets", struct{}{}, &res); err != nil {
			return nil, err
		}
		wire = res.Datasets
	}
	out := make([]connector.Dataset, len(wire))
	for i, d := range wire {
		out[i] = toDataset(d)
	}
	return out, nil
}

// Resolve returns the schema of a dataset.
func (c *Connector) Resolve(ctx context.Context, ds connector.Dataset) (engine.Schema, error) {
	c.mu.Lock()
	if err := c.ensureStarted(ctx); err != nil {
		c.mu.Unlock()
		return engine.Schema{}, err
	}
	cl := c.cl
	c.mu.Unlock()

	var res resolveResult
	if err := cl.call("resolve", resolveParams{Dataset: toWireDataset(ds)}, &res); err != nil {
		return engine.Schema{}, err
	}
	return toSchema(res.Schema), nil
}

// Scan opens a cursor on the plugin and returns an iterator that pulls row
// batches via `next`. Pushdown hints are sent only for capabilities the plugin
// advertised; everything the plugin does not apply is re-applied by the engine.
func (c *Connector) Scan(ctx context.Context, req connector.ScanRequest) (engine.RowIterator, error) {
	c.mu.Lock()
	if err := c.ensureStarted(ctx); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	caps := c.info.Capabilities
	cl := c.cl
	c.mu.Unlock()

	params := scanParams{Dataset: toWireDataset(req.Dataset)}
	if caps.ProjectionPushdown {
		params.Columns = req.Columns
	}
	if caps.LimitPushdown {
		params.Limit = req.Limit
	}
	if caps.PredicatePushdown && req.Predicate != nil {
		params.Predicate = encodePredicate(req.Predicate)
	}
	if caps.OrderByPushdown {
		for _, t := range req.OrderBy {
			params.OrderBy = append(params.OrderBy, wireOrderTerm{Column: t.Column, Desc: t.Desc})
		}
	}

	var res scanResult
	if err := cl.call("scan", params, &res); err != nil {
		return nil, err
	}
	return &scanIter{
		cl:      cl,
		scanID:  res.ScanID,
		schema:  toSchema(res.Schema),
		maxRows: defaultBatchRows,
	}, nil
}

// Close shuts the plugin down: it sends a shutdown notification, closes stdin,
// and waits briefly before killing the process.
func (c *Connector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return nil
	}
	c.started = false
	return c.stop()
}

// stop terminates the subprocess. Caller holds c.mu.
func (c *Connector) stop() error {
	if c.cl != nil {
		_ = c.cl.notify("shutdown", struct{}{})
	}
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
		return nil
	}
}

// ---- conversions -------------------------------------------------------------

func toWireDataset(ds connector.Dataset) wireDataset {
	return wireDataset{Name: ds.Name, Source: ds.Source, Options: ds.Options}
}

func toDataset(d wireDataset) connector.Dataset {
	return connector.Dataset{Name: d.Name, Source: d.Source, Options: d.Options}
}

// ---- scan iterator -----------------------------------------------------------

// scanIter pulls row batches from the plugin via `next`, decoding each cell
// against the scan schema, and releases the cursor via `close`.
type scanIter struct {
	cl      *client
	scanID  string
	schema  engine.Schema
	maxRows int

	batch  []engine.Row
	i      int
	done   bool
	closed bool
}

func (it *scanIter) Next() (engine.Row, bool, error) {
	for it.i >= len(it.batch) {
		if it.done {
			return engine.Row{}, false, nil
		}
		if err := it.fetch(); err != nil {
			return engine.Row{}, false, err
		}
	}
	r := it.batch[it.i]
	it.i++
	return r, true, nil
}

func (it *scanIter) fetch() error {
	var res nextResult
	if err := it.cl.call("next", nextParams{ScanID: it.scanID, MaxRows: it.maxRows}, &res); err != nil {
		return err
	}
	rows := make([]engine.Row, len(res.Rows))
	for ri, cells := range res.Rows {
		vals := make([]engine.Value, len(it.schema.Columns))
		for ci := range it.schema.Columns {
			if ci < len(cells) {
				v, err := decodeCell(cells[ci], it.schema.Columns[ci].Type)
				if err != nil {
					return fmt.Errorf("scan column %q: %w", it.schema.Columns[ci].Name, err)
				}
				vals[ci] = v
			} else {
				vals[ci] = engine.Null() // short row: pad with NULLs
			}
		}
		rows[ri] = engine.Row{Values: vals}
	}
	it.batch = rows
	it.i = 0
	it.done = res.Done
	return nil
}

func (it *scanIter) Close() error {
	if it.closed {
		return nil
	}
	it.closed = true
	return it.cl.call("close", closeParams{ScanID: it.scanID}, nil)
}
