package termsafe_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/termsafe"
)

func TestTextDropsWhatATerminalWouldObey(t *testing.T) {
	cases := map[string]string{
		"plain :-) https://example.com":          "plain :-) https://example.com",
		"colour \x1b[31mred\x1b[0m done":         "colour red done",
		"title \x1b]0;pwned\x07 after":           "title  after",
		"cursor \x1b[2J\x1b[H gone":              "cursor  gone",
		"c1 \u009b31m and \u0085 gone":           "c1  and  gone",
		"bell\x07 and null\x00 and del\x7f":      "bell and null and del",
		"tab\tkept as space":                     "tab kept as space",
		"bidi ‮evil‬ ​zw":                        "bidi evil zw",
		"unicode ☕ 🚀 stays":                      "unicode ☕ 🚀 stays",
		"bad utf8 \xff\xfe fixed":                "bad utf8 � fixed",
		"string terminator \x1b]0;t\x1b\\ after": "string terminator  after",
		"dcs \x1bPq#0\x1b\\ gone":                "dcs  gone",
		"charset \x1b(B kept":                    "charset  kept",
	}
	for in, want := range cases {
		if got := termsafe.Text(in); got != want {
			t.Errorf("Text(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTextIsBounded(t *testing.T) {
	long := strings.Repeat("x", 10_000)
	if got := termsafe.Text(long); len(got) != termsafe.MaxCols {
		t.Fatalf("width bound: got %d columns, want %d", len(got), termsafe.MaxCols)
	}
	tall := strings.Repeat("y\n", 1_000)
	got := termsafe.Text(tall)
	if n := strings.Count(got, "\n"); n > termsafe.MaxLines {
		t.Fatalf("line bound: %d newlines", n)
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Fatal("a truncated value must say so")
	}
}

func TestANSIKeepsColourOnlyAndAlwaysResets(t *testing.T) {
	in := "a \x1b[1;31mred\x1b[0m b \x1b[2J c \x1b]0;t\x07 d"
	got := termsafe.Sanitize(in, termsafe.Options{ANSI: true})
	want := "a \x1b[1;31mred\x1b[0m b  c  d\x1b[0m"
	if got != want {
		t.Fatalf("ansi: got %q, want %q", got, want)
	}
	if got := termsafe.Sanitize("plain", termsafe.Options{ANSI: true}); got != "plain" {
		t.Fatalf("no colour, no reset: %q", got)
	}
	long := "\x1b[" + strings.Repeat("1;", 30) + "m x"
	if got := termsafe.Sanitize(long, termsafe.Options{ANSI: true}); strings.Contains(got, "\x1b[1;") {
		t.Fatalf("an over-long SGR must be dropped: %q", got)
	}
}

func TestASCIIDegradesAboveSevenBits(t *testing.T) {
	if got := termsafe.Sanitize("café ☕ :-)", termsafe.Options{ASCII: true}); got != "caf? ? :-)" {
		t.Fatalf("got %q", got)
	}
}

// Half-block art draws a cell's second pixel with the background colour.
// Stripping colour must not strip that pixel: a half block on a set
// background becomes a full block, so the plain rendering keeps the shape.
// With ANSI nothing changes, because the colour is there to draw it.
func TestPlainRenderingKeepsHalfBlockShape(t *testing.T) {
	plain := termsafe.Options{}
	cases := []struct{ in, want string }{
		// Both pixels inked: top in fg, bottom in bg.
		{"\x1b[38;5;196;48;5;160m▀\x1b[0m", "█"},
		// Only the top pixel: no background, the glyph is already right.
		{"\x1b[38;5;196m▀\x1b[0m", "▀"},
		// Lower half drawn in fg over a background: both inked.
		{"\x1b[38;5;250;48;5;245m▄", "█"},
		// A reset between cells clears the background.
		{"\x1b[48;5;160m▀\x1b[0m▀", "█▀"},
		// 49 is "default background".
		{"\x1b[48;5;160m▀\x1b[49m▀", "█▀"},
		// Basic and bright background codes.
		{"\x1b[41m▀\x1b[m▀\x1b[101m▄", "█▀█"},
		// Direct colour.
		{"\x1b[48;2;239;0;4m▀", "█"},
		// THE TRAP: 41 here is a FOREGROUND palette index, not background
		// code 41. Reading it as a code would ink a cell that has no
		// background at all.
		{"\x1b[38;5;41m▀", "▀"},
		{"\x1b[38;2;41;41;41m▀", "▀"},
		// Text and spaces on a coloured panel are left alone: flattening a
		// space would turn a coloured bar into a wall of ink.
		{"\x1b[48;5;160m A \x1b[0m", " A "},
	}
	for _, c := range cases {
		if got := termsafe.Sanitize(c.in, plain); got != c.want {
			t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// And ANSI passes the original through, colour and glyph both.
	in := "\x1b[38;5;196;48;5;160m▀"
	if got := termsafe.Sanitize(in, termsafe.Options{ANSI: true}); !strings.Contains(got, "▀") || strings.Contains(got, "█") {
		t.Errorf("ANSI changed the glyph: %q", got)
	}
}

func TestValidateBoundedNamesTheOffence(t *testing.T) {
	ok := []string{"hello\nworld\t:-) https://example.com ☕", "\x1b[32mgreen\x1b[0m", ""}
	for _, s := range ok {
		if err := termsafe.ValidateBounded("plan", s); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
	bad := map[string]string{
		"a\x07b":                   "plan line 1 has a control character (U+0007)",
		"x\n\x1b[2Jclear":          "plan line 2 has an escape sequence that is not a colour (SGR)",
		"\x1b]0;title\x07":         "plan line 1 has an escape sequence that is not a colour (SGR)",
		"x‮y":                      "plan line 1 has a zero-width or bidirectional character (U+202E)",
		"x­y":                      "plan line 1 has a non-printable character (U+00AD)",
		"bad\xffutf":               "plan is not valid UTF-8",
		strings.Repeat("l\n", 201): "plan has more than 200 lines",
		strings.Repeat("w", 513):   "plan line 1 is wider than 512 columns",
	}
	for s, want := range bad {
		err := termsafe.ValidateBounded("plan", s)
		if err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %q", s, err, want)
			continue
		}
		if !errors.Is(err, termsafe.ErrUnsafe) {
			t.Errorf("%q: %v does not match ErrUnsafe", s, err)
		}
	}
}

// The display bounds are for a value meant to be read on a screen. Content
// meant to be longer than one passes Validate, which still refuses anything
// Sanitize would strip.
func TestValidateHasNoDisplayBounds(t *testing.T) {
	long := strings.Repeat("line\n", termsafe.MaxLines*3) + strings.Repeat("w", termsafe.MaxCols*2)
	if err := termsafe.Validate("doc", long); err != nil {
		t.Fatalf("a long document was refused: %v", err)
	}
	if err := termsafe.ValidateBounded("doc", long); err == nil {
		t.Fatal("the bounded check let a long document through")
	}
	err := termsafe.Validate("doc", "bad\x07bell")
	if err == nil || err.Error() != "doc line 1 has a control character (U+0007)" {
		t.Fatalf("a control character: %v", err)
	}
}

// An application puts its own prefix in front of the refusal and keeps both
// matches.
func TestValidateErrorWrapsUnderAnApplicationSentinel(t *testing.T) {
	errApp := errors.New("body text")
	err := termsafe.ValidateBounded("plan", "a\x07b")
	wrapped := errors.Join(errApp, err)
	if !errors.Is(wrapped, errApp) || !errors.Is(wrapped, termsafe.ErrUnsafe) {
		t.Fatalf("%v lost a match", wrapped)
	}
}

// A key read from a file someone else wrote is not trusted to be the length
// of a key, or to be hex: a short one must not panic the message that names
// it, and one carrying an escape sequence must not reach the terminal.
func TestAbbrevNeverPanicsAndFilters(t *testing.T) {
	if got := termsafe.Abbrev(strings.Repeat("ab", 33)); got != strings.Repeat("ab", 6) {
		t.Errorf("a whole key: %q", got)
	}
	for _, in := range []string{"", "02ab", "\x1b]0;owned\x07", "02\x1b[2J"} {
		if got := termsafe.Abbrev(in); strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
			t.Errorf("%q reached the terminal as %q", in, got)
		}
	}
	if got := termsafe.Abbrev("ééééééééééééééé"); got != "éééééééééééé" {
		t.Errorf("cut by byte rather than rune: %q", got)
	}
}

func TestUTF8LocaleTakesTheFirstVariableSet(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	cases := []struct {
		name string
		vars map[string]string
		want bool
	}{
		{"nothing set", nil, true},
		{"LANG utf-8", map[string]string{"LANG": "en_US.UTF-8"}, true},
		{"LANG spelled utf8", map[string]string{"LANG": "C.utf8"}, true},
		{"LANG C", map[string]string{"LANG": "C"}, false},
		{"LC_ALL wins over LANG", map[string]string{"LC_ALL": "POSIX", "LANG": "en_US.UTF-8"}, false},
		{"LC_CTYPE wins over LANG", map[string]string{"LC_CTYPE": "en_US.UTF-8", "LANG": "C"}, true},
	}
	for _, c := range cases {
		if got := termsafe.UTF8Locale(env(c.vars)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
