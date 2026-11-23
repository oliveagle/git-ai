package classify

import (
	"testing"
)

// TestTokenizeCommentsAndNesting covers the comments of the lexers: the line
// and block comments of every language, the nested block comments of Rust and
// the doc comments that are code rather than comments.
func TestTokenizeCommentsAndNesting(t *testing.T) {
	for _, tc := range []struct {
		name string
		lang language
		src  string
		want []string
	}{
		{"go line comment", langGo, "x := 1 // one", []string{"x", ":=", "1"}},
		{"go block comment", langGo, "x := 1 /* one */ + 2", []string{"x", ":=", "1", "+", "2"}},
		{"java line comment", langJava, "int x = 1; // one", []string{"int", "x", "=", "1", ";"}},
		{"java block comment", langJava, "int x = 1; /* one */", []string{"int", "x", "=", "1", ";"}},
		{"java doc comment is code", langJava, "/** the doc */\nint x;", []string{"/** the doc */", "int", "x", ";"}},
		{"rust nested block comment", langRust, "let x = 1; /* a /* b */ c */", []string{"let", "x", "=", "1", ";"}},
		{"rust line doc comment is code", langRust, "/// the doc\nlet x;", []string{"/// the doc", "let", "x", ";"}},
		{"python hash comment", langPython, "x = 1  # one", []string{"\x01", "x", "=", "1"}},
		{"python triple string with a hash", langPython, `x = """a # b"""`, []string{"\x01", "x", "=", `"""a # b"""`}},
		{"go raw string with a comment marker", langGo, "x := `// not a comment`", []string{"x", ":=", "`// not a comment`"}},
		{"rust raw string with a comment marker", langRust, "let s = r#\"// not a comment\"#;", []string{"let", "s", "=", `r#"// not a comment"#`, ";"}},
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
