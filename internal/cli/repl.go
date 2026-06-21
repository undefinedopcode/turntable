package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/render"
	"golang.org/x/term"
)

// historyFile is the path used to persist REPL line history.
const historyFile = ".octoparser_history"

// replPrompt is printed before each input line.
const replPrompt = "octo> "

// runREPL drives the interactive read-eval-print loop. It returns the process
// exit code. When stdin is not a TTY it falls back to a simple line reader so
// piped input still works (e.g. `echo 'SELECT 1' | octoparser --repl`).
func (a *App) repl(ctx context.Context) int {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return a.replBatch(ctx, os.Stdin)
	}

	hist, err := loadHistory()
	if err != nil {
		hist = nil // non-fatal
	}

	old, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Fprintf(a.Err, "repl: cannot enter raw mode: %v\n", err)
		return 1
	}
	defer term.Restore(int(os.Stdin.Fd()), old)

	fmt.Fprint(a.Out, replBanner())
	r := newLineEditor(os.Stdin, a.Out, a.completions())
	r.history = hist

	for {
		fmt.Fprint(r.out, replPrompt)
		line, ok := r.readLine()
		if !ok { // Ctrl-D / EOF
			fmt.Fprintln(r.out)
			break
		}
		// Remember non-blank lines (minus the prompt echo).
		if strings.TrimSpace(line) != "" {
			r.history = append(r.history, line)
		}
		if r.lastWasCtrlC {
			r.lastWasCtrlC = false
			fmt.Fprintln(r.out, "^C")
			continue
		}
		// A line may be a dot-command or a SQL fragment. SQL fragments are
		// accumulated until a terminating semicolon.
		handled, quit, err := a.handleReplLine(ctx, line, r)
		if err != nil {
			fmt.Fprintf(r.out, "error: %v\n", err)
		}
		if quit {
			break
		}
		_ = handled
	}
	_ = saveHistory(r.history)
	return 0
}

// replBatch reads whole lines from r (non-interactive), handling dot-commands
// and accumulating SQL until a semicolon, then running each statement.
func (a *App) replBatch(ctx context.Context, r io.Reader) int {
	br := newLineReader(r)
	var buf strings.Builder
	for {
		fmt.Fprint(a.Out, replPrompt)
		line, ok := br.readLine()
		if !ok {
			break
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, ".") {
			if quit := a.dotCommand(ctx, trimmed, a.Out); quit {
				return 0
			}
			continue
		}
		buf.WriteString(" ")
		buf.WriteString(line)
		if strings.HasSuffix(trimmed, ";") {
			q := strings.TrimSpace(buf.String())
			buf.Reset()
			q = strings.TrimSuffix(q, ";")
			if q != "" {
				a.runQuery(ctx, q, a.replExplain, true)
			}
		}
	}
	return 0
}

// handleReplLine processes a single input line within the interactive editor.
// It returns handled=true if the line was consumed as a dot-command or a
// completed SQL statement, and quit=true if the user asked to exit.
func (a *App) handleReplLine(ctx context.Context, line string, r *lineEditor) (handled, quit bool, err error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true, false, nil
	}
	if strings.HasPrefix(trimmed, ".") {
		q := a.dotCommand(ctx, trimmed, r.out)
		return true, q, nil
	}
	// Accumulate SQL until a terminating semicolon.
	r.pending.WriteString(" ")
	r.pending.WriteString(line)
	if !strings.HasSuffix(trimmed, ";") {
		return true, false, nil
	}
	q := strings.TrimSpace(r.pending.String())
	r.pending.Reset()
	q = strings.TrimSuffix(q, ";")
	if q == "" {
		return true, false, nil
	}
	a.runQuery(ctx, q, a.replExplain, true)
	return true, false, nil
}

// dotCommand executes a REPL meta-command beginning with ".". It returns true
// if the command requests termination (.quit / .exit).
func (a *App) dotCommand(ctx context.Context, line string, out io.Writer) bool {
	args := strings.Fields(line)
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case ".quit", ".exit":
		return true
	case ".help":
		fmt.Fprintln(out, replHelp())
	case ".tables":
		a.cmdTables(out)
	case ".schema":
		name := ""
		if len(args) >= 2 {
			name = args[1]
		}
		a.cmdSchema(ctx, name, out)
	case ".output", ".format":
		if len(args) >= 2 {
			if r, err := render.New(render.Format(args[1])); err == nil {
				_ = r
				a.Output = render.Format(args[1])
				fmt.Fprintf(out, "output format: %s\n", a.Output)
			} else {
				fmt.Fprintf(out, "%v\n", err)
			}
		} else {
			fmt.Fprintf(out, "usage: .output <table|csv|json|ndjson|yaml|raw>\n")
		}
	case ".explain":
		if len(args) >= 2 && strings.EqualFold(args[1], "off") {
			a.replExplain = false
			fmt.Fprintln(out, "explain mode: off")
		} else {
			a.replExplain = !a.replExplain
			fmt.Fprintf(out, "explain mode: %s\n", onOff(a.replExplain))
		}
	case ".strict":
		if len(args) >= 2 && strings.EqualFold(args[1], "off") {
			a.strict = false
			fmt.Fprintln(out, "strict mode: off")
		} else {
			a.strict = !a.strict
			fmt.Fprintf(out, "strict mode: %s\n", onOff(a.strict))
		}
	case ".sources":
		a.cmdTables(out)
	default:
		fmt.Fprintf(out, "unknown command %q; type .help\n", args[0])
	}
	return false
}

// cmdTables lists all registered logical sources.
func (a *App) cmdTables(out io.Writer) {
	srcs := a.Reg.Sources()
	if len(srcs) == 0 {
		fmt.Fprintln(out, "(no sources registered; use -c <config>)")
		return
	}
	names := make([]string, 0, len(srcs))
	byName := map[string]connector.Source{}
	for _, s := range srcs {
		names = append(names, s.Name)
		byName[s.Name] = s
	}
	sort.Strings(names)
	w := 0
	for _, n := range names {
		if len(n) > w {
			w = len(n)
		}
	}
	for _, n := range names {
		s := byName[n]
		fmt.Fprintf(out, "%-*s  (%s)\n", w, n, connectorName(s))
	}
}

// cmdSchema prints the resolved schema for a named source, or all sources when
// name is empty.
func (a *App) cmdSchema(ctx context.Context, name string, out io.Writer) {
	srcs := a.Reg.Sources()
	if name != "" {
		s, ok := a.Reg.Resolve(name)
		if !ok {
			fmt.Fprintf(out, "no source named %q\n", name)
			return
		}
		srcs = []connector.Source{s}
	}
	for _, s := range srcs {
		schema, err := s.Conn.Resolve(ctx, s.Dataset)
		if err != nil {
			fmt.Fprintf(out, "%s: <error: %v>\n", s.Name, err)
			continue
		}
		fmt.Fprintf(out, "%s:\n", s.Name)
		if len(schema.Columns) == 0 {
			fmt.Fprintln(out, "  (no columns)")
			continue
		}
		for _, c := range schema.Columns {
			nullable := ""
			if c.Nullable {
				nullable = "?"
			}
			fmt.Fprintf(out, "  %-20s %s%s\n", c.Name, c.Type, nullable)
		}
	}
}

// connectorName returns the connector's short prefix for display.
func connectorName(s connector.Source) string {
	if s.Conn != nil {
		return s.Conn.Name()
	}
	return "?"
}

// completions returns the tab-completion candidates for the REPL: dot-commands
// and registered source names.
func (a *App) completions() []string {
	cands := []string{
		".help", ".tables", ".schema", ".output", ".quit", ".exit", ".explain", ".strict", ".sources",
	}
	for _, s := range a.Reg.Sources() {
		cands = append(cands, s.Name)
	}
	return cands
}

func replBanner() string {
	return "octoparser REPL — type .help for commands, .quit to exit\n"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func replHelp() string {
	return `Commands:
  .tables                 list registered sources
  .schema [name]          show columns for a source (or all)
  .output <fmt>           set output format (table|csv|json|ndjson|yaml|raw)
  .explain [off]          toggle explain mode
  .strict [off]           toggle strict type-checking mode
  .help                   this message
  .quit | .exit           exit

Type a SQL query ending with ; to run it. Multi-line input is supported.`
}

// ---- history persistence -----------------------------------------------------

func loadHistory() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(home, historyFile))
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func saveHistory(lines []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Cap history length.
	if len(lines) > 1000 {
		lines = lines[len(lines)-1000:]
	}
	return os.WriteFile(filepath.Join(home, historyFile), []byte(strings.Join(lines, "\n")), 0o600)
}

// ---- minimal line reader (non-interactive) -----------------------------------

type simpleReader struct {
	r   *strings.Reader
	src io.Reader
	eof bool
	buf []byte
}

func newLineReader(r io.Reader) *simpleReader {
	return &simpleReader{src: r}
}

func (s *simpleReader) readLine() (string, bool) {
	if s.eof {
		return "", false
	}
	for {
		i := -1
		for idx, b := range s.buf {
			if b == '\n' {
				i = idx
				break
			}
		}
		if i >= 0 {
			line := string(s.buf[:i])
			s.buf = s.buf[i+1:]
			return strings.TrimRight(line, "\r"), true
		}
		tmp := make([]byte, 4096)
		n, err := s.src.Read(tmp)
		if n > 0 {
			s.buf = append(s.buf, tmp[:n]...)
		}
		if err != nil {
			if len(s.buf) > 0 {
				line := string(s.buf)
				s.buf = nil
				s.eof = true
				return strings.TrimRight(line, "\r"), true
			}
			s.eof = true
			return "", false
		}
	}
}
