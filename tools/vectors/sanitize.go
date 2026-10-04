package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// The renderer filter, written here a second time from its four rules, so
// that the corpus's outputs come from code that shares nothing with the
// library's: this one reads the parsed Unicode files, never the generated
// table, and scans escape sequences with its own functions.

type oracle struct{ d *emojiData }

func inRanges(rs [][2]rune, r rune) bool {
	for _, x := range rs {
		if r >= x[0] && r <= x[1] {
			return true
		}
	}
	return false
}

func (o oracle) pictographic(r rune) bool { return inRanges(o.d.pictographic, r) }

// decodeBytes is the WHATWG UTF-8 decoder (Encoding Standard, section 9.1.1),
// which TextDecoder implements: each maximal ill-formed subpart becomes one
// U+FFFD.
func decodeBytes(b []byte) []rune {
	var out []rune
	i := 0
	for i < len(b) {
		c := b[i]
		var need int
		var lo, hi byte = 0x80, 0xBF
		var v rune
		switch {
		case c < 0x80:
			out = append(out, rune(c))
			i++
			continue
		case c >= 0xC2 && c <= 0xDF:
			need, v = 1, rune(c&0x1F)
		case c >= 0xE0 && c <= 0xEF:
			need, v = 2, rune(c&0x0F)
			if c == 0xE0 {
				lo = 0xA0
			} else if c == 0xED {
				hi = 0x9F
			}
		case c >= 0xF0 && c <= 0xF4:
			need, v = 3, rune(c&0x07)
			if c == 0xF0 {
				lo = 0x90
			} else if c == 0xF4 {
				hi = 0x8F
			}
		default:
			out = append(out, 0xFFFD)
			i++
			continue
		}
		j := i + 1
		ok := true
		for k := 0; k < need; k++ {
			if j >= len(b) || b[j] < lo || b[j] > hi {
				ok = false
				break
			}
			v = v<<6 | rune(b[j]&0x3F)
			lo, hi = 0x80, 0xBF
			j++
		}
		if !ok {
			out = append(out, 0xFFFD)
			i = j // the byte that broke the sequence is read again
			continue
		}
		out = append(out, v)
		i = j
	}
	return out
}

// Rule 1: a tab is a space, U+2028 and U+2029 are LF.
func rule1(rs []rune) []rune {
	out := make([]rune, len(rs))
	for i, r := range rs {
		switch r {
		case '\t':
			r = ' '
		case 0x2028, 0x2029:
			r = '\n'
		}
		out[i] = r
	}
	return out
}

func isC0C1(r rune) bool { return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) }

// invisible is rule 2's fixed list: the bidirectional controls, the
// zero-width and invisible characters, and the supplementary variation
// selectors.
func invisible(r rune) bool {
	for _, x := range [][2]rune{
		{0x061C, 0x061C}, {0x200E, 0x200F}, {0x202A, 0x202E}, {0x2066, 0x2069},
		{0x00AD, 0x00AD}, {0x034F, 0x034F}, {0x115F, 0x1160}, {0x180E, 0x180E},
		{0x200B, 0x200C}, {0x2060, 0x2064}, {0x3164, 0x3164}, {0xFEFF, 0xFEFF}, {0xFFA0, 0xFFA0},
		{0xE0100, 0xE01EF},
	} {
		if r >= x[0] && r <= x[1] {
			return true
		}
	}
	return false
}

// endOfEscape returns the index just past the escape sequence that starts
// at i (an ESC or a C1 CSI), as termsafe reads one: a CSI runs to a final
// byte (0x40 to 0x7E) or a control; OSC, DCS, SOS, PM and APC to BEL, ST
// (U+009C) or ESC \, where ESC followed by anything else is passed over
// with what follows it; ESC with intermediates (0x20 to 0x2F) to a byte
// from 0x30 to 0x7E or a control; ESC and any other character is those two.
// A sequence the value ends inside runs to the end.
func endOfEscape(rs []rune, i int) int {
	j := i + 1
	if rs[i] == 0x9B {
		return endCSI(rs, j)
	}
	if j >= len(rs) {
		return j
	}
	switch c := rs[j]; {
	case c == '[':
		return endCSI(rs, j+1)
	case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
		for k := j + 1; k < len(rs); k++ {
			switch rs[k] {
			case 0x07, 0x9C:
				return k + 1
			case 0x1B:
				k++
				if k < len(rs) && rs[k] == '\\' {
					return k + 1
				}
			}
		}
		return len(rs)
	case c >= 0x20 && c <= 0x2F:
		for k := j + 1; k < len(rs); k++ {
			if (rs[k] >= 0x30 && rs[k] <= 0x7E) || rs[k] < 0x20 {
				return k + 1
			}
		}
		return len(rs)
	default:
		return j + 1
	}
}

func endCSI(rs []rune, k int) int {
	for ; k < len(rs); k++ {
		if (rs[k] >= 0x40 && rs[k] <= 0x7E) || rs[k] < 0x20 {
			return k + 1
		}
	}
	return len(rs)
}

// Rule 2: controls but LF, escape sequences, the fixed invisible list, and
// tag characters outside a listed tag sequence with the character kept
// before them.
func (o oracle) rule2(rs []rune) []rune {
	var out []rune
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case r == 0x1B || r == 0x9B:
			i = endOfEscape(rs, i)
		case r == '\n':
			out = append(out, r)
			i++
		case isC0C1(r) || invisible(r):
			i++
		case isTag(r):
			j := i
			for j < len(rs) && isTag(rs[j]) {
				j++
			}
			if len(out) > 0 {
				seq := append([]rune{out[len(out)-1]}, rs[i:j]...)
				if slices.ContainsFunc(o.d.tagSeqs, func(s []rune) bool { return slices.Equal(s, seq) }) {
					out = append(out, rs[i:j]...)
				}
			}
			i = j
		default:
			out = append(out, r)
			i++
		}
	}
	return out
}

// Rule 3: U+FE00 to U+FE0D go; U+FE0E and U+FE0F stay only after a base the
// variation sequences list for them.
func (o oracle) rule3(rs []rune) []rune {
	var out []rune
	for _, r := range rs {
		switch {
		case r >= 0xFE00 && r <= 0xFE0D:
			continue
		case r == 0xFE0E || r == 0xFE0F:
			bases := o.d.textVS
			if r == 0xFE0F {
				bases = o.d.emojiVS
			}
			if len(out) == 0 || !slices.Contains(bases, out[len(out)-1]) {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// Rule 4: U+200D stays only between two Extended_Pictographic characters,
// the one before found by passing over U+FE0F and emoji modifiers.
func (o oracle) rule4(rs []rune) []rune {
	var out []rune
	for i, r := range rs {
		if r == 0x200D {
			k := len(out) - 1
			for k >= 0 && (out[k] == 0xFE0F || inRanges(o.d.modifier, out[k])) {
				k--
			}
			if k < 0 || !o.pictographic(out[k]) || i+1 >= len(rs) || !o.pictographic(rs[i+1]) {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

func (o oracle) filter(b []byte) []byte {
	return []byte(string(o.rule4(o.rule3(o.rule2(rule1(decodeBytes(b)))))))
}

type sanitizeCase struct {
	Name      string `json:"name"`
	InputHex  string `json:"inputHex"`
	OutputHex string `json:"outputHex"`
}

// sanitizeVector is the renderer filter's corpus: inputs as bytes, since
// some are not UTF-8, and the bytes the filter returns for each, pinned to
// the table by its SHA-256.
type sanitizeVector struct {
	Unicode     string         `json:"unicode"`
	TableSHA256 string         `json:"tableSha256"`
	Cases       []sanitizeCase `json:"cases"`
}

const (
	england   = "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F"
	scotland  = "\U0001F3F4\U000E0067\U000E0062\U000E0073\U000E0063\U000E0074\U000E007F"
	wales     = "\U0001F3F4\U000E0067\U000E0062\U000E0077\U000E006C\U000E0073\U000E007F"
	heartFire = "\u2764\uFE0F\u200D\U0001F525"
	rainbow   = "\U0001F3F3\uFE0F\u200D\U0001F308"
)

// sanitizeCorpus is every case: a name, the input, and, where the rules
// alone fix it, the output the generator must reach, which it checks
// before it writes anything.
var sanitizeCorpus = []struct {
	name, in string
	want     *string
}{
	{"plain ASCII", "hello, world", nil},
	{"empty", "", nil},
	{"text outside ASCII: Latin, Greek, CJK, Arabic, a combining accent", "caf\u00E9 \u03B1\u03B2\u03B3 \u4F60\u597D \u0645\u0631\u062D\u0628\u0627 e\u0301", nil},
	{"a tab becomes a space", "a\tb\t\tc", ptr("a b  c")},
	{"LF is kept", "one\ntwo\n", ptr("one\ntwo\n")},
	{"U+2028 becomes LF", "one\u2028two", ptr("one\ntwo")},
	{"U+2029 becomes LF", "one\u2029two", ptr("one\ntwo")},
	{"CR and CRLF: CR goes", "a\rb\r\nc", ptr("ab\nc")},
	{"NUL, BEL, BS, VT, FF and DEL go", "a\x00b\x07c\x08d\x0be\x0cf\x7fg", ptr("abcdefg")},
	{"C1 controls go: NEL, U+0080, U+009F", "a\u0085b\u0080c\u009fd", ptr("abcd")},
	{"a colour sequence goes, its text stays", "\x1b[31mred\x1b[0m plain", ptr("red plain")},
	{"clear screen and cursor home go", "\x1b[2J\x1b[Htext", ptr("text")},
	{"a window title, ended by BEL", "\x1b]0;pwned\x07after", ptr("after")},
	{"a window title, ended by ST as ESC \\", "\x1b]0;pwned\x1b\\after", ptr("after")},
	{"a window title, ended by U+009C", "\x1b]0;pwned\u009cafter", ptr("after")},
	{"a hyperlink sequence and its text", "\x1b]8;;https://example.com\x07link\x1b]8;;\x07", ptr("link")},
	{"an OSC the value ends inside takes the rest", "before\x1b]0;never ended", ptr("before")},
	{"a DCS string", "a\x1bPq#0;2;0;0;0\x1b\\b", ptr("ab")},
	{"ESC with an intermediate: a charset switch", "a\x1b(Bb", ptr("ab")},
	{"ESC and one character: a terminal reset", "a\x1bcb", ptr("ab")},
	{"a lone ESC at the end", "abc\x1b", ptr("abc")},
	{"C1 CSI, U+009B, as a colour sequence", "a\u009b31mb", ptr("ab")},
	{"a CSI cut by a control takes the control", "a\x1b[12\nb", ptr("ab")},
	{"bidi: right-to-left override and pop", "abc\u202Edcb\u202C", ptr("abcdcb")},
	{"bidi: every control in the list", "a\u061Cb\u200Ec\u200Fd\u202Ae\u202Bf\u202Cg\u202Dh\u202Ei\u2066j\u2067k\u2068l\u2069m", ptr("abcdefghijklm")},
	{"a disguised identifier, isolates around a comment", "access\u2067 \u2066// check later\u2069 \u2066granted", ptr("access // check later granted")},
	{"zero width space, non-joiner, word joiner", "a\u200Bb\u200Cc\u2060d", ptr("abcd")},
	{"invisible operators U+2061 to U+2064", "a\u2061b\u2062c\u2063d\u2064e", ptr("abcde")},
	{"soft hyphen and combining grapheme joiner", "ex\u00ADtra\u034F", ptr("extra")},
	{"Hangul fillers", "\u115F\u1160a\u3164b\uFFA0", ptr("ab")},
	{"Mongolian vowel separator", "a\u180Eb", ptr("ab")},
	{"a byte order mark, leading and inside", "\uFEFFa\uFEFFb", ptr("ab")},
	{"supplementary variation selectors, first and last", "a\U000E0100b\U000E01EFc", ptr("abc")},
	{"the flag of England is kept whole", england, ptr(england)},
	{"the flag of Scotland is kept whole", scotland, ptr(scotland)},
	{"the flag of Wales is kept whole", wales, ptr(wales)},
	{"three flags in text", "go " + england + scotland + wales + "!", ptr("go " + england + scotland + wales + "!")},
	{"a tag run the table does not list: its tags go, the black flag stays", "\U0001F3F4\U000E0067\U000E0062\U000E007A\U000E007A\U000E007A\U000E007F", ptr("\U0001F3F4")},
	{"England without its cancel tag", "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067", ptr("\U0001F3F4")},
	{"England with a tag too many", "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E0067\U000E007F", ptr("\U0001F3F4")},
	{"England's tags after a letter: hidden text goes", "a\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F", ptr("a")},
	{"tag characters spelling a hidden instruction", "hi\U000E0069\U000E0067\U000E006E\U000E006F\U000E0072\U000E0065", ptr("hi")},
	{"tag characters at the start", "\U000E0041\U000E0042x", ptr("x")},
	{"U+E0001, the language tag", "\U000E0001\U000E0065\U000E006Ex", ptr("x")},
	{"a flag whose tags an escape sequence interrupts", "\U0001F3F4\x1b[0m\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F", ptr(england)},
	{"heart on fire is kept", heartFire, ptr(heartFire)},
	{"heart on fire without its U+FE0F", "\u2764\u200D\U0001F525", ptr("\u2764\u200D\U0001F525")},
	{"the rainbow flag is kept", rainbow, ptr(rainbow)},
	{"the transgender flag", "\U0001F3F3\uFE0F\u200D\u26A7\uFE0F", ptr("\U0001F3F3\uFE0F\u200D\u26A7\uFE0F")},
	{"a family of three", "\U0001F468\u200D\U0001F469\u200D\U0001F467", ptr("\U0001F468\u200D\U0001F469\u200D\U0001F467")},
	{"a skin tone", "\U0001F44D\U0001F3FD", ptr("\U0001F44D\U0001F3FD")},
	{"every skin tone", "\U0001F44B\U0001F3FB\U0001F44B\U0001F3FC\U0001F44B\U0001F3FD\U0001F44B\U0001F3FE\U0001F44B\U0001F3FF", nil},
	{"a skin tone before a joiner", "\U0001F469\U0001F3FD\u200D\U0001F4BB", ptr("\U0001F469\U0001F3FD\u200D\U0001F4BB")},
	{"two skin tones across a joiner", "\U0001FAF1\U0001F3FB\u200D\U0001FAF2\U0001F3FF", ptr("\U0001FAF1\U0001F3FB\u200D\U0001FAF2\U0001F3FF")},
	{"a joiner between letters goes", "a\u200Db", ptr("ab")},
	{"a joiner after an emoji, before a letter", "\U0001F525\u200Da", ptr("\U0001F525a")},
	{"a joiner before an emoji, after a letter", "a\u200D\U0001F525", ptr("a\U0001F525")},
	{"a joiner at each end", "\u200D\U0001F525\u200D", ptr("\U0001F525")},
	{"two joiners between emoji: one stays", "\U0001F525\u200D\u200D\U0001F525", ptr("\U0001F525\u200D\U0001F525")},
	{"a joiner before a skin tone", "\U0001F44D\u200D\U0001F3FD", ptr("\U0001F44D\U0001F3FD")},
	{"a joiner after a lone skin tone", "\U0001F3FD\u200D\U0001F525", ptr("\U0001F3FD\U0001F525")},
	{"a run of U+FE0F: one stays", "\u2764\uFE0F\uFE0F\uFE0F", ptr("\u2764\uFE0F")},
	{"U+FE0E then U+FE0F: the first stays", "\u2764\uFE0E\uFE0F", ptr("\u2764\uFE0E")},
	{"U+FE0E after a base that lists it", "\u263A\uFE0E", ptr("\u263A\uFE0E")},
	{"U+FE0F after a letter goes", "a\uFE0F", ptr("a")},
	{"U+FE0F after an emoji the table does not list goes", "\U0001F600\uFE0F", ptr("\U0001F600")},
	{"U+FE00 to U+FE0D go", "a\uFE00b\uFE07c\uFE0Dd", ptr("abcd")},
	{"a selector run hiding bits after a letter", "x\uFE00\uFE01\uFE02\uFE0E\uFE0F\uFE0D", ptr("x")},
	{"a keycap: digit, U+FE0F, U+20E3", "1\uFE0F\u20E3#\uFE0F\u20E3", ptr("1\uFE0F\u20E3#\uFE0F\u20E3")},
	{"a selector after a removed zero-width space", "\u2764\u200B\uFE0F", ptr("\u2764\uFE0F")},
	{"a selector after a removed U+FE00", "\u2764\uFE00\uFE0F", ptr("\u2764\uFE0F")},
	{"U+FE0F after a flag's tags goes", england + "\uFE0F", ptr(england)},
	{"invalid UTF-8: a lone continuation byte", "a\x80b", ptr("a\uFFFDb")},
	{"invalid UTF-8: 0xFF", "a\xffb", ptr("a\uFFFDb")},
	{"invalid UTF-8: a three-byte sequence cut short", "a\xe2\x82b", ptr("a\uFFFDb")},
	{"invalid UTF-8: an overlong NUL", "a\xc0\x80b", ptr("a\uFFFD\uFFFDb")},
	{"invalid UTF-8: an encoded surrogate", "a\xed\xa0\x80b", ptr("a\uFFFD\uFFFD\uFFFDb")},
	{"invalid UTF-8: above U+10FFFF", "a\xf4\x90\x80\x80b", ptr("a\uFFFD\uFFFD\uFFFD\uFFFDb")},
	{"invalid UTF-8: a four-byte sequence cut at the end", "a\xf0\x9f\x94", ptr("a\uFFFD")},
	{"invalid UTF-8 inside an escape sequence", "\x1b[\xff31mx", ptr("x")},
	{"private use and noncharacters are not on the list", "\uE000\uFFFF\U0010FFFF", ptr("\uE000\uFFFF\U0010FFFF")},
	{"a value that is only what the rules remove", "\x1b[0m\u200B\u202E\u2066\U000E0041\uFE0F\x00", ptr("")},
	{"a mixed message", "Hi @team \u2764\uFE0F\u200D\U0001F525\t\x1b[1mship it\x1b[0m\u202E!\u2028" + england, ptr("Hi @team " + heartFire + " ship it!\n" + england)},
}

func ptr(s string) *string { return &s }

func sanitizeFamilies(repo string) ([]family, error) {
	files, d, err := tableFiles(repo)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(files[tableGo])
	o := oracle{d}
	v := sanitizeVector{Unicode: unicodeVersion, TableSHA256: hex.EncodeToString(sum[:])}
	seen := map[string]bool{}
	for _, c := range sanitizeCorpus {
		if seen[c.name] {
			return nil, fmt.Errorf("two cases named %q", c.name)
		}
		seen[c.name] = true
		got := o.filter([]byte(c.in))
		if c.want != nil && string(got) != *c.want {
			return nil, fmt.Errorf("%s: the filter gives %+q, the case wants %+q", c.name, got, *c.want)
		}
		if strings.ContainsRune(string(got), 0x1B) {
			return nil, fmt.Errorf("%s: an ESC survived", c.name)
		}
		v.Cases = append(v.Cases, sanitizeCase{Name: c.name, InputHex: hex.EncodeToString([]byte(c.in)), OutputHex: hex.EncodeToString(got)})
	}
	return []family{{"sanitize-v1.json", v}}, nil
}
