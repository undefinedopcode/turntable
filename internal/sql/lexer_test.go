package sql

import "testing"

// firstURL returns the value of the first TKURL token, or "" if none.
func firstURL(toks []Token) string {
	for _, t := range toks {
		if t.Kind == TKURL {
			return t.Value
		}
	}
	return ""
}

func TestLexURLBasic(t *testing.T) {
	toks, err := Lex("SELECT * FROM http://example.com/users.json")
	if err != nil {
		t.Fatalf("Lex error: %v", err)
	}
	if got := firstURL(toks); got != "http://example.com/users.json" {
		t.Errorf("url token = %q", got)
	}
}

func TestLexURLQueryStringPreserved(t *testing.T) {
	// Query-string characters (?, &, =, %) must stay inside the URL token, even
	// though '=' is an operator and 'true' is a keyword elsewhere.
	toks, err := Lex("SELECT * FROM https://api.test/v1/items?active=true&limit=5&q=a%20b")
	if err != nil {
		t.Fatalf("Lex error: %v", err)
	}
	want := "https://api.test/v1/items?active=true&limit=5&q=a%20b"
	if got := firstURL(toks); got != want {
		t.Errorf("url token = %q, want %q", got, want)
	}
}

func TestLexURLTerminatesAtWhitespace(t *testing.T) {
	toks, err := Lex("SELECT * FROM http://h/p AS feed WHERE x = 1")
	if err != nil {
		t.Fatalf("Lex error: %v", err)
	}
	if got := firstURL(toks); got != "http://h/p" {
		t.Errorf("url token = %q, want http://h/p", got)
	}
	// The alias and WHERE must remain their own tokens.
	var sawAS, sawWhere bool
	for _, tk := range toks {
		if tk.Kind == TKKeyword && tk.Value == "AS" {
			sawAS = true
		}
		if tk.Kind == TKKeyword && tk.Value == "WHERE" {
			sawWhere = true
		}
	}
	if !sawAS || !sawWhere {
		t.Errorf("expected AS and WHERE tokens after URL (AS=%v WHERE=%v)", sawAS, sawWhere)
	}
}

func TestLexURLTerminators(t *testing.T) {
	// Note: a bare ";" is a URL terminator too, but the lexer rejects a
	// standalone ";" elsewhere (the REPL strips a trailing ";" before lexing),
	// so it can't be exercised through Lex directly.
	cases := map[string]string{
		"FROM http://h/a, csv:./b":  "http://h/a", // comma-separated FROM
		"FROM (http://h/a)":         "http://h/a", // closing paren
		"FROM http://h/a WHERE z>1": "http://h/a", // whitespace
	}
	for src, want := range cases {
		toks, err := Lex(src)
		if err != nil {
			t.Errorf("Lex(%q) error: %v", src, err)
			continue
		}
		if got := firstURL(toks); got != want {
			t.Errorf("Lex(%q): url = %q, want %q", src, got, want)
		}
	}
}

func TestLexPrefixedDSNURL(t *testing.T) {
	// sql:postgres://... lexes as ident(sql), op(:), url(postgres://...).
	toks, err := Lex("SELECT * FROM sql:postgres://user@host:5432/db")
	if err != nil {
		t.Fatalf("Lex error: %v", err)
	}
	if got := firstURL(toks); got != "postgres://user@host:5432/db" {
		t.Errorf("url token = %q", got)
	}
}

func TestLexNotAURL(t *testing.T) {
	// A double slash without a preceding "scheme:" is not a URL — `a / / b`
	// divides; no URL token should appear.
	toks, err := Lex("SELECT a / b FROM t")
	if err != nil {
		t.Fatalf("Lex error: %v", err)
	}
	if got := firstURL(toks); got != "" {
		t.Errorf("unexpected url token %q", got)
	}
	// The existing prefix:path form (csv:./x) must not become a URL token.
	toks, _ = Lex("SELECT * FROM csv:./data/sales.csv")
	if got := firstURL(toks); got != "" {
		t.Errorf("csv:./ ref wrongly lexed as URL: %q", got)
	}
}
