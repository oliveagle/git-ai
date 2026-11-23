package classify

import (
	"strings"
	"testing"
)

// TestTokenizeCertainEdgeCases covers the corners of the lexers that the
// stream tests do not reach: the literals whose prefix, escape or nesting the
// language spells in its own way. Every source here has certain token
// boundaries, so the lexer must answer without a reason.
func TestTokenizeCertainEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		lang language
		src  string
		want []string
	}{
		{"java empty string", langJava, `s = "";`, []string{"s", "=", `""`, ";"}},
		{"java text block", langJava, "s = \"\"\"\n x\n \"\"\";", []string{"s", "=", "\"\"\"\n x\n \"\"\"", ";"}},
		{"go float that starts with a dot", langGo, "x := .5", []string{"x", ":=", ".5"}},
		{"java float that starts with a dot", langJava, "x = .5;", []string{"x", "=", ".5", ";"}},
		{"rust dot before a digit is a field", langRust, "t.0", []string{"t", ".", "0"}},
		{"go raw string", langGo, "x := `a`", []string{"x", ":=", "`a`"}},
		{"rust character", langRust, "let c = 'a';", []string{"let", "c", "=", "'a'", ";"}},
		{"rust escaped character", langRust, `let c = '\'';`, []string{"let", "c", "=", `'\''`, ";"}},
		{"rust lifetime", langRust, "struct S<'a> { x: &'a u8 }", []string{"struct", "S", "<", "'a", ">", "{", "x", ":", "&", "'a", "u8", "}"}},
		{"rust raw string", langRust, `let s = r"x";`, []string{"let", "s", "=", `r"x"`, ";"}},
		{"rust raw string with hashes", langRust, `let s = r#"x"#;`, []string{"let", "s", "=", `r#"x"#`, ";"}},
		{"rust byte raw string", langRust, `let s = br"x";`, []string{"let", "s", "=", `br"x"`, ";"}},
		{"rust c raw string", langRust, `let s = cr"x";`, []string{"let", "s", "=", `cr"x"`, ";"}},
		{"rust byte string", langRust, `let s = b"x";`, []string{"let", "s", "=", `b"x"`, ";"}},
		{"rust c string", langRust, `let s = c"x";`, []string{"let", "s", "=", `c"x"`, ";"}},
		{"rust byte character", langRust, `let c = b'x';`, []string{"let", "c", "=", "b'x'", ";"}},
		{"rust raw identifier", langRust, "let r#type = 1;", []string{"let", "r#type", "=", "1", ";"}},
		{"rust string across lines", langRust, "let s = \"a\nb\";", []string{"let", "s", "=", "\"a\nb\"", ";"}},
		{"python single quoted string", langPython, "x = 'a'", []string{"\x01", "x", "=", "'a'"}},
		{"python raw string", langPython, "x = r'a'", []string{"\x01", "x", "=", "r'a'"}},
		{"python byte string", langPython, "x = b'a'", []string{"\x01", "x", "=", "b'a'"}},
		{"python f string without a field", langPython, "x = f'a'", []string{"\x01", "x", "=", "f'a'"}},
		{"python triple quoted string", langPython, "x = '''a'''", []string{"\x01", "x", "=", "'''a'''"}},
		{"python double quoted triple string", langPython, `x = """a"""`, []string{"\x01", "x", "=", `"""a"""`}},
		{"python continuation with a windows line break", langPython, "x \\\r\n  y", []string{"\x01", "x", "\\\n", "y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason, ok := tokenize(tc.lang, []byte(tc.src))
			if !ok {
				t.Fatalf("tokenize not certain: %s", reason)
			}
			if !equalTokens(got, tc.want) {
				t.Fatalf("tokens = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTokenizeUncertainEdgeCases covers the sources whose token boundaries the
// lexer cannot decide: a directive that the compiler reads, a literal whose
// end is missing and a prefix that joins two tokens.
func TestTokenizeUncertainEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		name string
		lang language
		src  string
	}{
		{"go build directive", langGo, "//go:build linux\n"},
		{"go export directive", langGo, "//export Foo\n"},
		{"go noinline directive in a block comment", langGo, "/*go:noinline*/\n"},
		{"go cgo import", langGo, `import "C"`},
		{"go unterminated raw string", langGo, "x := `a"},
		{"rust unterminated raw string", langRust, `let s = r"x`},
		{"rust unterminated byte string", langRust, `let s = b"x`},
		{"rust unterminated byte character", langRust, `let c = b'`},
		{"rust unclear raw prefix", langRust, "let r# = 1;"},
		{"rust identifier before a quote", langRust, `let x"y" = 1;`},
		{"rust unterminated quote", langRust, "'"},
		{"rust unterminated nested block comment", langRust, "/* /* */"},
		{"rust literal with a suffix", langRust, `let s = "x"s;`},
		{"python unterminated double quoted triple string", langPython, `x = """a""`},
		{"python unterminated triple string with two quotes", langPython, "x = '''a''"},
		{"python f string with a field", langPython, "x = f'a{b}'"},
		{"python shebang", langPython, "#!/usr/bin/env python\n"},
		{"python encoding declaration", langPython, "# -*- coding: utf-8 -*-\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, reason, ok := tokenize(tc.lang, []byte(tc.src))
			if ok {
				t.Fatal("tokenize must be uncertain")
			}
			if reason == "" {
				t.Fatal("an uncertain answer must say why")
			}
		})
	}
}

// TestGoSpecialComment covers the prefixes the compiler reads. A text that is
// not a comment at all is not one either.
func TestGoSpecialComment(t *testing.T) {
	for _, tc := range []struct {
		name string
		rest string
		want bool
	}{
		{"go build directive", "//go:build linux", true},
		{"export directive", "//export Foo", true},
		{"line directive", "//line 3", true},
		{"build tag", "// +build linux", true},
		{"extern block", "/*extern int x;", true},
		{"plain comment", "// a comment", false},
		{"plain block comment", "/* a comment */", false},
		{"not a comment", "x", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := goSpecialComment([]byte(tc.rest)); got != tc.want {
				t.Fatalf("goSpecialComment(%q) = %v, want %v", tc.rest, got, tc.want)
			}
		})
	}
}

// TestTokenizeReasonsAreReadable checks that every reason of an uncertain
// answer names the construct that stopped the lexer, so that a reviewer can
// tell a code change from a spelling change.
func TestTokenizeReasonsAreReadable(t *testing.T) {
	for _, tc := range []struct {
		lang language
		src  string
		want string
	}{
		{langGo, "//go:build linux\n", "Go directive comment"},
		{langGo, "x := `a", "unterminated raw string"},
		{langRust, `let s = r"x`, "unterminated raw string"},
		{langRust, `let s = b"x`, "unterminated string"},
		{langRust, "let r# = 1;", "unclear raw literal"},
		{langRust, `let x"y" = 1;`, "identifier directly followed by a quote"},
		{langRust, "'", "unterminated quote"},
		{langRust, "/* /* */", "unterminated block comment"},
		{langPython, "#!/usr/bin/env python\n", "meaningful # comment"},
		{langPython, "x = f'a{b}'", "f-string with replacement fields"},
		{langPython, "x \\ \ny", "backslash not at the end of a line"},
	} {
		_, reason, ok := tokenize(tc.lang, []byte(tc.src))
		if ok {
			t.Fatalf("%q must be uncertain", tc.src)
		}
		if !strings.Contains(reason, tc.want) {
			t.Fatalf("reason of %q = %q, want it to mention %q", tc.src, reason, tc.want)
		}
	}
}
