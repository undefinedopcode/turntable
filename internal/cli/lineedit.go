package cli

import (
	"io"
	"strings"
)

// lineEditor is a minimal raw-mode line editor supporting:
//   - left/right cursor movement, backspace, delete
//   - up/down history navigation with in-place line replacement
//   - Ctrl-C (cancel current line), Ctrl-D (EOF on empty line)
//   - tab completion against a candidate list (longest common prefix or
//     single match)
//
// It reads one byte at a time from in and renders to out. The terminal must
// already be in raw mode.
type lineEditor struct {
	in        io.Reader
	out       io.Writer
	buf       []rune
	pos       int
	history   []string
	histIdx   int // index into history for navigation; -1 = current line
	compl     []string
	maxRows   int

	// pending accumulates a multi-line SQL statement until a terminator.
	pending strings.Builder

	// lastWasCtrlC is set when the user pressed Ctrl-C so the caller can emit ^C.
	lastWasCtrlC bool
}

func newLineEditor(in io.Reader, out io.Writer, completions []string) *lineEditor {
	return &lineEditor{
		in:      in,
		out:     out,
		compl:   completions,
		histIdx: -1,
	}
}

// readLine reads one logical input line, returning the text (without newline)
// and ok=false on EOF (Ctrl-D on an empty buffer).
func (e *lineEditor) readLine() (string, bool) {
	e.buf = e.buf[:0]
	e.pos = 0
	e.histIdx = -1

	for {
		var b [1]byte
		n, err := e.in.Read(b[:])
		if err != nil || n == 0 {
			if len(e.buf) > 0 {
				return string(e.buf), true
			}
			return "", false
		}
		c := b[0]
		switch c {
		case '\r', '\n':
			writeStr(e.out, "\r\n")
			return string(e.buf), true
		case 0x03: // Ctrl-C
			e.lastWasCtrlC = true
			return "", true
		case 0x04: // Ctrl-D
			if len(e.buf) == 0 {
				return "", false
			}
			// Delete char under cursor (like readline).
			if e.pos < len(e.buf) {
				e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
				e.redraw()
			}
		case 0x7f, 0x08: // Backspace / Ctrl-H
			if e.pos > 0 {
				e.pos--
				e.buf = append(e.buf[:e.pos], e.buf[e.pos+1:]...)
				e.redraw()
			}
		case 0x01: // Ctrl-A: home
			e.pos = 0
			e.redraw()
		case 0x05: // Ctrl-E: end
			e.pos = len(e.buf)
			e.redraw()
		case 0x0b: // Ctrl-K: kill to end
			e.buf = e.buf[:e.pos]
			e.redraw()
		case 0x15: // Ctrl-U: kill to start
			e.buf = append(e.buf[:0], e.buf[e.pos:]...)
			e.pos = 0
			e.redraw()
		case 0x09: // Tab
			e.complete()
		case 0x1b: // Escape sequence (arrow keys etc.)
			e.handleEscape()
		default:
			if c < 0x20 {
				continue // ignore other control chars
			}
			e.insertRune(rune(c))
		}
	}
}

// handleEscape reads a 2-byte escape sequence and applies cursor/history motion.
func (e *lineEditor) handleEscape() {
	var seq [2]byte
	n, _ := e.in.Read(seq[:])
	if n < 2 {
		return
	}
	if seq[0] != '[' {
		return
	}
	switch seq[1] {
	case 'C': // right
		if e.pos < len(e.buf) {
			e.pos++
			e.redraw()
		}
	case 'D': // left
		if e.pos > 0 {
			e.pos--
			e.redraw()
		}
	case 'A': // up: previous history
		e.histPrev()
	case 'B': // down: next history
		e.histNext()
	case 'H': // Home
		e.pos = 0
		e.redraw()
	case 'F': // End
		e.pos = len(e.buf)
		e.redraw()
	}
}

func (e *lineEditor) insertRune(r rune) {
	e.buf = append(e.buf, 0)
	copy(e.buf[e.pos+1:], e.buf[e.pos:])
	e.buf[e.pos] = r
	e.pos++
	e.redraw()
}

// redraw re-renders the current line: moves to column 0, clears the line,
// writes the buffer, then positions the cursor.
func (e *lineEditor) redraw() {
	// \r moves to start; \x1b[K clears from cursor to end of line.
	writeStr(e.out, "\r\x1b[K")
	writeRunes(e.out, e.buf)
	// Move cursor back to pos.
	back := len(e.buf) - e.pos
	if back > 0 {
		writeStr(e.out, "\x1b["+itoa(back)+"D")
	}
}

func (e *lineEditor) histPrev() {
	if len(e.history) == 0 {
		return
	}
	if e.histIdx == -1 {
		e.histIdx = len(e.history) - 1
	} else if e.histIdx > 0 {
		e.histIdx--
	} else {
		return
	}
	e.buf = []rune(e.history[e.histIdx])
	e.pos = len(e.buf)
	e.redraw()
}

func (e *lineEditor) histNext() {
	if e.histIdx == -1 {
		return
	}
	if e.histIdx < len(e.history)-1 {
		e.histIdx++
		e.buf = []rune(e.history[e.histIdx])
	} else {
		e.histIdx = -1
		e.buf = e.buf[:0]
	}
	e.pos = len(e.buf)
	e.redraw()
}

// complete performs tab completion against the candidate list. If there is a
// unique match it substitutes it; otherwise it extends the prefix by the
// longest common prefix of matching candidates.
func (e *lineEditor) complete() {
	// Determine the word being completed (from the cursor back to whitespace).
	start := e.pos
	for start > 0 && !isWordBreak(e.buf[start-1]) {
		start--
	}
	prefix := string(e.buf[start:e.pos])
	if prefix == "" {
		return
	}
	var matches []string
	for _, c := range e.compl {
		if strings.HasPrefix(strings.ToLower(c), strings.ToLower(prefix)) {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return
	}
	if len(matches) == 1 {
		e.replaceWord(start, e.pos, matches[0])
		return
	}
	lcp := matches[0]
	for _, m := range matches[1:] {
		lcp = commonPrefix(lcp, m)
	}
	if lcp == prefix {
		// No further extension possible; print the candidates as a hint.
		writeStr(e.out, "\r\n")
		writeStr(e.out, strings.Join(matches, "  "))
		writeStr(e.out, "\r\n")
		e.redraw()
		return
	}
	e.replaceWord(start, e.pos, lcp)
}

func (e *lineEditor) replaceWord(start, end int, replacement string) {
	e.buf = append(e.buf[:start], append([]rune(replacement), e.buf[end:]...)...)
	e.pos = start + len([]rune(replacement))
	e.redraw()
}

func isWordBreak(r rune) bool {
	switch r {
	case ' ', '\t', ',', '(', ')', ';':
		return true
	}
	return false
}

func commonPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[:i]
		}
	}
	return a[:n]
}

// ---- tiny output helpers -----------------------------------------------------

func writeStr(w io.Writer, s string) {
	_, _ = w.Write([]byte(s))
}

func writeRunes(w io.Writer, rs []rune) {
	_, _ = w.Write([]byte(string(rs)))
}

// itoa is a small strconv-free int->string for cursor movement escapes.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// crlfWriter wraps an io.Writer and rewrites every bare '\n' as "\r\n". In raw
// terminal mode the TTY does not perform on-output LF→CRLF translation, so a
// plain newline only moves the cursor down one row without returning to column
// 0. Routing all interactive output through this writer fixes that for every
// fmt.Fprint/Fprintln/Fprintf call and for the renderers without touching them.
type crlfWriter struct {
	w       io.Writer
	lastCR  bool // tracks whether the previous byte was '\r' (to avoid doubling)
}

func (c *crlfWriter) Write(p []byte) (int, error) {
	// Fast path: no newlines, pass through unchanged.
	if !hasByte(p, '\n') {
		n, err := c.w.Write(p)
		if err == nil {
			c.lastCR = len(p) > 0 && p[len(p)-1] == '\r'
		}
		return n, err
	}
	var out []byte
	for _, b := range p {
		if b == '\n' && !c.lastCR {
			out = append(out, '\r')
		}
		out = append(out, b)
		c.lastCR = b == '\r'
	}
	if _, err := c.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

func hasByte(p []byte, b byte) bool {
	for _, x := range p {
		if x == b {
			return true
		}
	}
	return false
}
