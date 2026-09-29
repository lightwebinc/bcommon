// Package termsafe filters text someone else wrote before it reaches a
// terminal, and checks text an application is about to publish against the
// same rules.
//
// A terminal obeys the escape sequences and control characters it is sent.
// Text read from a record, a log line or a store was written by whoever
// published it, so printing it raw lets that publisher drive the reader's
// terminal: set the window title, move the cursor, clear the screen, or make
// one string look like another with bidirectional overrides. Sanitize passes
// printable text, spaces and newlines, turns a tab into a space, drops every
// control character, every escape sequence and the zero-width and
// bidirectional characters, and bounds the result at MaxLines lines of
// MaxCols columns, so one value cannot scroll a terminal indefinitely.
// Validate and ValidateBounded are the publishing side of the same rules:
// they name the first thing in a value that Sanitize would have to strip.
//
// Unlike the rest of this module, this package is about terminals. It still
// reads no environment of its own accord: an application that wants to know
// whether the locale draws UTF-8 passes its own lookup to UTF8Locale.
package termsafe

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLines and MaxCols bound what Sanitize returns: at most MaxLines lines,
// each of at most MaxCols printed runes. ValidateBounded holds a value to the
// same two bounds.
const (
	MaxLines = 200
	MaxCols  = 512
)

// sgrMax bounds the parameter bytes of an SGR sequence that is passed
// through: enough for any colour or weight, too short for anything cute.
const sgrMax = 32

// Options is how a value may reach the terminal.
type Options struct {
	// ANSI keeps colour and weight (SGR) sequences, and nothing else. Set it
	// only when the reader asked for colour: the sequences are re-validated
	// rather than copied, and a reset is always written at the end.
	ANSI bool
	// ASCII degrades every rune above 0x7E to "?", for a terminal that
	// cannot draw it.
	ASCII bool
}

// Text is Sanitize with no options: what a pipe, a log or a label gets.
func Text(s string) string { return Sanitize(s, Options{}) }

// Sanitize returns s with only printable runes, spaces, tabs (as spaces) and
// newlines, bounded at MaxLines lines of MaxCols columns. A value cut at the
// line bound ends with "[truncated]". It never returns a byte sequence a
// terminal interprets, except an SGR sequence when o.ANSI is set, which is
// re-validated here rather than copied and is always followed by a reset: at
// the end, or before "[truncated]" when the value is cut.
//
// Invalid UTF-8 becomes U+FFFD. Without o.ANSI, a half block drawn on a set
// background becomes a full block: half-block art inks a cell's second half
// with the background colour, and dropping the colour would otherwise break
// the picture into stripes, where a full block keeps its shape.
func Sanitize(s string, o Options) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	var b strings.Builder
	b.Grow(len(s))
	lines, cols := 0, 0
	var params strings.Builder
	coloured := false
	// bgSet tracks whether the SGR state being stripped has a background
	// colour set; see the half-block case below.
	bgSet := false
	const (
		text     = iota
		afterEsc // ESC seen, next rune decides the sequence kind
		csi      // ESC [ ... final byte 0x40..0x7E
		str      // OSC, DCS, APC, PM, SOS: until BEL or ST
		strEsc   // ESC inside a string: ESC \ is ST
		twoChar  // ESC + intermediates + one final byte 0x30..0x7E
	)
	state := text
	for _, r := range s {
		switch state {
		case afterEsc:
			switch {
			case r == '[':
				state = csi
			case r == ']' || r == 'P' || r == 'X' || r == '^' || r == '_':
				state = str
			case r >= 0x20 && r <= 0x2F:
				state = twoChar
			default:
				state = text
			}
			continue
		case csi:
			switch {
			case r >= '0' && r <= '9' || r == ';':
				if params.Len() <= sgrMax {
					params.WriteRune(r)
				}
				continue
			case r == 'm' && params.Len() <= sgrMax:
				if o.ANSI {
					b.WriteString("\x1b[" + params.String() + "m")
					coloured = true
				} else {
					bgSet = sgrBackground(params.String(), bgSet)
				}
			}
			if r >= 0x40 && r <= 0x7E || r < 0x20 {
				state = text
			}
			params.Reset()
			continue
		case str:
			if r == 0x07 || r == 0x9C {
				state = text
			} else if r == 0x1B {
				state = strEsc
			}
			continue
		case strEsc:
			if r == '\\' {
				state = text
			} else {
				state = str
			}
			continue
		case twoChar:
			if r >= 0x30 && r <= 0x7E || r < 0x20 {
				state = text
			}
			continue
		}
		switch {
		case r == '\n':
			lines++
			if lines >= MaxLines {
				// The reset the end of the value would have written, written
				// here instead, so a colour cut off mid-value does not run on
				// into the marker or whatever the caller prints next.
				if coloured {
					b.WriteString("\x1b[0m")
				}
				b.WriteString("\n[truncated]")
				return b.String()
			}
			cols = 0
			b.WriteRune(r)
		case r == '\t':
			b.WriteRune(' ')
			cols++
		case r == 0x1B:
			state = afterEsc
		case r == 0x9B:
			// A lone CSI byte is a CSI without the ESC.
			params.Reset()
			state = csi
		case r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F):
			// C0 and C1 controls.
		case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0xFEFF:
			// Zero-width and bidirectional-override characters.
		case !unicode.IsPrint(r) && r != ' ':
		default:
			if cols >= MaxCols {
				continue
			}
			if !o.ANSI && bgSet && (r == '▀' || r == '▄') {
				// Half-block art draws two pixels per cell: the glyph's half
				// in the foreground colour and the other half in the
				// BACKGROUND colour. Dropping colour keeps the glyph and
				// loses the background, so every cell with both pixels
				// inked collapses to one and the picture breaks into
				// stripes. A half block drawn on a set background is a
				// full cell of ink, so it is written as one, and the shape
				// survives as a silhouette when the colour cannot.
				r = '█'
			}
			if o.ASCII && r > 0x7E {
				b.WriteByte('?')
			} else {
				b.WriteRune(r)
			}
			cols++
		}
	}
	if coloured {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// sgrBackground reports whether a background colour is set after applying
// one SGR parameter string to the state bg. Only the background is tracked,
// and only so that the plain rendering can keep half-block art's shape.
//
// Extended colours are consumed whole, because their operands are not
// codes: in 38;5;41 the 41 is a foreground palette index, and reading it as
// background code 41 would ink cells that are not.
func sgrBackground(params string, bg bool) bool {
	if params == "" {
		return false // ESC [ m is a reset
	}
	ps := strings.Split(params, ";")
	for i := 0; i < len(ps); i++ {
		n, err := strconv.Atoi(ps[i])
		if err != nil {
			continue
		}
		switch {
		case n == 0 || n == 49:
			bg = false
		case n >= 40 && n <= 47, n >= 100 && n <= 107:
			bg = true
		case n == 48 || n == 38:
			// 5;N is a palette index, 2;R;G;B a direct colour.
			skip := 0
			if i+1 < len(ps) {
				switch ps[i+1] {
				case "5":
					skip = 2
				case "2":
					skip = 4
				}
			}
			if n == 48 && skip > 0 {
				bg = true
			}
			i += skip
		}
	}
	return bg
}

// Abbrev is the first twelve characters of s, filtered by Text: the form a
// message names a key by, from its hex. A key read from a file someone else
// wrote may be shorter than that, or not hex at all, so it is cut by rune,
// which never panics, and filtered like any other text from outside.
func Abbrev(s string) string {
	if r := []rune(s); len(r) > 12 {
		s = string(r[:12])
	}
	return Text(s)
}

// UTF8Locale reports whether the locale says the terminal draws UTF-8, from
// the first of LC_ALL, LC_CTYPE and LANG that getenv answers with a value. A
// locale that says nothing is taken as UTF-8. The application passes its own
// lookup, os.Getenv for the process's environment, so this package reads no
// environment of its own.
func UTF8Locale(getenv func(string) string) bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := getenv(k); v != "" {
			u := strings.ToUpper(strings.ReplaceAll(v, "-", ""))
			return strings.Contains(u, "UTF8")
		}
	}
	return true
}

// ErrUnsafe matches, through errors.Is, every error Validate and
// ValidateBounded return.
var ErrUnsafe = errors.New("termsafe: text a terminal would have to be protected from")

// unsafeText is a refusal from Validate. Its text starts with the field it
// names, so an application can put its own prefix in front and keep
// errors.Is on ErrUnsafe.
type unsafeText string

func (e unsafeText) Error() string { return string(e) }

func (unsafeText) Is(target error) bool { return target == ErrUnsafe }

// Validate reports the first thing in s that Sanitize would have to strip:
// invalid UTF-8, a control character, an escape sequence other than a colour
// (SGR) sequence, a zero-width or bidirectional character, or another
// non-printable character. Printable text, spaces, tabs and newlines pass,
// at any length. field names s in the error, which begins with it: "plan
// line 3 has a control character (U+0007)".
func Validate(field, s string) error { return validate(field, s, false) }

// ValidateBounded is Validate that also holds s to MaxLines lines of MaxCols
// columns. The two bounds are display bounds, not content rules: they suit a
// value meant to be read on a screen, and Sanitize applies them to whatever
// it prints anyway, so content that is meant to be longer than a screen
// belongs with Validate.
func ValidateBounded(field, s string) error { return validate(field, s, true) }

func validate(field, s string, bounded bool) error {
	if !utf8.ValidString(s) {
		return unsafeText(fmt.Sprintf("%s is not valid UTF-8", field))
	}
	lines, cols := 1, 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\n':
			lines++
			cols = 0
			if bounded && lines > MaxLines {
				return unsafeText(fmt.Sprintf("%s has more than %d lines", field, MaxLines))
			}
			continue
		case r == '\t':
		case r == 0x1B:
			// Only ESC [ <digits and semicolons> m may pass.
			j := i + 1
			if j >= len(rs) || rs[j] != '[' {
				return unsafeText(fmt.Sprintf("%s line %d has an escape sequence that is not a colour (SGR)", field, lines))
			}
			for j++; j < len(rs) && (rs[j] >= '0' && rs[j] <= '9' || rs[j] == ';'); j++ {
			}
			if j >= len(rs) || rs[j] != 'm' || j-i-2 > sgrMax {
				return unsafeText(fmt.Sprintf("%s line %d has an escape sequence that is not a colour (SGR)", field, lines))
			}
			i = j
			continue
		case r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F):
			return unsafeText(fmt.Sprintf("%s line %d has a control character (U+%04X)", field, lines, r))
		case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0xFEFF:
			return unsafeText(fmt.Sprintf("%s line %d has a zero-width or bidirectional character (U+%04X)", field, lines, r))
		case !unicode.IsPrint(r) && r != ' ':
			return unsafeText(fmt.Sprintf("%s line %d has a non-printable character (U+%04X)", field, lines, r))
		}
		cols++
		if bounded && cols > MaxCols {
			return unsafeText(fmt.Sprintf("%s line %d is wider than %d columns", field, lines, MaxCols))
		}
	}
	return nil
}
