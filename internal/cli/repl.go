package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/april/octoparser/internal/connector"
	"github.com/april/octoparser/internal/render"
	"github.com/chzyer/readline"
)

// historyFile is the path used to persist REPL line history.
const historyFile = ".octoparser_history"

// replPrompt is printed before each input line.
const replPrompt = "octo> "

// runREPL drives the interactive read-eval-print loop. It returns the process
// exit code. When stdin is not a TTY it falls back to a simple line reader so
// piped input still works (e.g. `echo 'SELECT 1' | octoparser --repl`).
func (a *App) repl(ctx context.Context) int {
	if !readline.IsTerminal(int(os.Stdin.Fd())) {
		return a.replBatch(ctx, os.Stdin)
	}

	// readline owns raw-mode terminal handling, line editing, history, tab
	// completion, and CRLF output translation — so there is nothing for us to
	// get wrong at the terminal level. Query results and errors are written to
	// the readline instance's Stdout/Stderr, which it keeps in sync with the
	// raw TTY.
	rl, err := readline.NewEx(&readline.Config{
		Prompt:            replPrompt,
		HistoryFile:       historyPath(),
		HistorySearchFold: true,
		AutoComplete:      a.replCompleter(),
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(a.Err, "repl: %v\n", err)
		return 1
	}
	defer rl.Close()

	fmt.Fprintln(rl.Stdout(), replBanner())
	var pending strings.Builder

	for {
		// Use a continuation prompt when accumulating a multi-line statement.
		if pending.Len() > 0 {
			rl.SetPrompt(replContPrompt)
		} else {
			rl.SetPrompt(replPrompt)
		}
		line, rerr := rl.Readline()
		if rerr == io.EOF { // Ctrl-D
			fmt.Fprintln(rl.Stdout())
			break
		}
		if rerr == readline.ErrInterrupt {
			// Ctrl-C: cancel any pending multi-line input and start fresh.
			pending.Reset()
			fmt.Fprintln(rl.Stdout(), "^C")
			continue
		}
		if rerr != nil {
			fmt.Fprintf(rl.Stderr(), "read error: %v\n", rerr)
			break
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Dot-commands are handled immediately and never join pending SQL.
		if strings.HasPrefix(trimmed, ".") {
			if a.dotCommand(ctx, trimmed, rl.Stdout()) {
				break
			}
			continue
		}
		// Accumulate SQL until a terminating semicolon.
		pending.WriteString(" ")
		pending.WriteString(line)
		if !strings.HasSuffix(trimmed, ";") {
			continue
		}
		q := strings.TrimSpace(pending.String())
		pending.Reset()
		q = strings.TrimSuffix(q, ";")
		if q != "" {
			a.runQueryInto(ctx, q, a.replExplain, true, rl.Stdout(), rl.Stderr())
		}
	}
	return 0
}

// replContPrompt is shown while a multi-line statement is being accumulated.
const replContPrompt = "   ...> "

// replCompleter builds a readline.AutoCompleter that completes dot-commands
// and registered source names against the current word.
func (a *App) replCompleter() readline.AutoCompleter {
	return &replCompleter{cands: a.completions()}
}

// replCompleter implements readline.AutoCompleter. Do returns candidate runes
// for the word under the cursor and an offset (0 = replace from word start).
type replCompleter struct {
	cands []string
}

func (c *replCompleter) Do(line []rune, pos int) ([][]rune, int) {
	start := pos
	for start > 0 && !isWordBreak(line[start-1]) {
		start--
	}
	prefix := strings.ToLower(string(line[start:pos]))
	if prefix == "" {
		return nil, 0
	}
	var matches [][]rune
	for _, cand := range c.cands {
		if strings.HasPrefix(strings.ToLower(cand), prefix) {
			matches = append(matches, []rune(cand))
		}
	}
	return matches, start
}

// isWordBreak reports whether r delimits words for completion.
func isWordBreak(r rune) bool {
	switch r {
	case ' ', '\t', ',', '(', ')', ';':
		return true
	}
	return false
}

// replBatch reads whole lines from r (non-interactive), handling dot-commands
// and accumulating SQL until a semicolon, then running each statement.
func (a *App) replBatch(ctx context.Context, r io.Reader) int {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var buf strings.Builder
	for {
		fmt.Fprint(a.Out, replPrompt)
		if !sc.Scan() {
			break
		}
		line := sc.Text()
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
				a.runQueryInto(ctx, q, a.replExplain, true, a.Out, a.Err)
			}
		}
	}
	return 0
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

// historyPath returns the on-disk path for REPL history. readline manages the
// file itself when HistoryFile is set, so we only need to name it.
func historyPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, historyFile)
}
