// Package sanitize filters text someone else wrote before a renderer shows
// it, in a terminal or on a web page, by one ordered set of character rules
// and one pinned Unicode property table, so that every implementation shows
// the same characters for the same value.
//
// Filter applies four rules, in order, each to what the one before it
// returned:
//
//  1. a tab becomes a space, and U+2028 and U+2029 become LF;
//  2. every control character but LF (C0, DEL and C1) goes, every escape
//     sequence goes whole, the bidirectional controls (U+061C, U+200E,
//     U+200F, U+202A to U+202E, U+2066 to U+2069), the zero-width and
//     invisible characters (U+00AD, U+034F, U+115F, U+1160, U+180E, U+200B,
//     U+200C, U+2060 to U+2064, U+3164, U+FEFF, U+FFA0) and the
//     supplementary variation selectors (U+E0100 to U+E01EF) go, and every
//     tag character (U+E0000 to U+E007F) goes unless its run, with the
//     character kept before it, is an emoji tag sequence the table lists
//     (the flags of England, Scotland and Wales), which stays whole;
//  3. U+FE00 to U+FE0D go, and U+FE0E or U+FE0F stays only where, with the
//     character kept before it, it forms an emoji variation sequence the
//     table lists, so at most one selector follows a character;
//  4. U+200D stays only where the nearest character kept before it, passing
//     over U+FE0F and emoji modifiers (U+1F3FB to U+1F3FF), and the
//     character after it are both Extended_Pictographic.
//
// "The character before" is always the last one the rule has kept, so a
// character a rule removes never stands between two it keeps. An escape
// sequence is what termsafe recognises as one: a CSI (ESC [ or U+009B) to
// its final byte, an OSC, DCS, SOS, PM or APC string to BEL, U+009C or
// ESC \, ESC with intermediates to its final byte, and ESC with any other
// one character; a sequence the value ends inside runs to the end.
//
// Invalid UTF-8 first becomes U+FFFD, one for each maximal ill-formed
// subsequence, as the WHATWG decoder (TextDecoder) replaces it, so a value
// read from bytes filters the same in both languages.
//
// The table is generated from Unicode 15.1's emoji data
// (emoji-data.txt, emoji-variation-sequences.txt and emoji-sequences.txt,
// under third_party/unicode) by tools/vectors, and is the same bytes the
// TypeScript package reads; neither uses its runtime's own Unicode
// properties. The filter neither bounds a value's length nor renders it: a
// terminal passes what it returns through termsafe, and a web page inserts
// it as text.
package sanitize

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"
)

// UnicodeVersion is the version of the Unicode data the table holds.
const UnicodeVersion = "15.1"

//go:embed unicode-15.1.json
var tableJSON []byte

type table struct {
	Unicode              string            `json:"unicode"`
	ExtendedPictographic [][2]rune         `json:"extendedPictographic"`
	EmojiModifier        [][2]rune         `json:"emojiModifier"`
	TextVariationBases   []rune            `json:"textVariationBases"`
	EmojiVariationBases  []rune            `json:"emojiVariationBases"`
	TagSequences         [][]rune          `json:"tagSequences"`
	Sources              map[string]string `json:"sources"`

	text, emoji map[rune]bool
	tags        map[string]bool
}

var tbl = mustTable()

func mustTable() *table {
	t := &table{}
	if err := json.Unmarshal(tableJSON, t); err != nil {
		panic(fmt.Sprintf("sanitize: the embedded table: %v", err))
	}
	if t.Unicode != UnicodeVersion {
		panic("sanitize: the embedded table is Unicode " + t.Unicode)
	}
	t.text, t.emoji, t.tags = map[rune]bool{}, map[rune]bool{}, map[string]bool{}
	for _, r := range t.TextVariationBases {
		t.text[r] = true
	}
	for _, r := range t.EmojiVariationBases {
		t.emoji[r] = true
	}
	for _, s := range t.TagSequences {
		t.tags[string(s)] = true
	}
	return t
}

func inRanges(rs [][2]rune, r rune) bool {
	i := sort.Search(len(rs), func(i int) bool { return rs[i][1] >= r })
	return i < len(rs) && rs[i][0] <= r
}

func pictographic(r rune) bool { return inRanges(tbl.ExtendedPictographic, r) }

func modifier(r rune) bool { return inRanges(tbl.EmojiModifier, r) }

// Filter returns s with the four rules applied. It never returns a control
// character but LF, nor any character rule 2 lists.
func Filter(s string) string {
	rs := decode(s)
	rs = mapSpaces(rs)
	rs = removeHidden(rs)
	rs = keepSelectors(rs)
	rs = keepJoiners(rs)
	return string(rs)
}

// decode reads s as UTF-8 the way the WHATWG decoder does: each maximal
// ill-formed subsequence (a lead byte and the continuation bytes it
// allows, up to the first it does not) is one U+FFFD, and the byte that
// ended it is read again.
func decode(s string) []rune {
	out := make([]rune, 0, len(s))
	if utf8.ValidString(s) {
		for _, r := range s {
			out = append(out, r)
		}
		return out
	}
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError || n > 1 {
			out = append(out, r)
			i += n
			continue
		}
		out = append(out, utf8.RuneError)
		i += illFormed(s[i:])
	}
	return out
}

// illFormed is the length of the maximal ill-formed subsequence at the
// start of s, which does not start with a valid encoding: at least one
// byte, and past the lead byte only the continuation bytes Unicode's table
// of well-formed sequences (Table 3-7) allows in their places.
func illFormed(s string) int {
	c := s[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c >= 0xE0 && c <= 0xEF:
		need = 2
		if c == 0xE0 {
			lo = 0xA0
		} else if c == 0xED {
			hi = 0x9F
		}
	case c >= 0xF0 && c <= 0xF4:
		need = 3
		if c == 0xF0 {
			lo = 0x90
		} else if c == 0xF4 {
			hi = 0x8F
		}
	default:
		return 1
	}
	n := 1
	for ; n <= need && n < len(s); n++ {
		if s[n] < lo || s[n] > hi {
			break
		}
		lo, hi = 0x80, 0xBF
	}
	return n
}

// mapSpaces is rule 1.
func mapSpaces(rs []rune) []rune {
	for i, r := range rs {
		switch r {
		case '\t':
			rs[i] = ' '
		case 0x2028, 0x2029:
			rs[i] = '\n'
		}
	}
	return rs
}

// hidden is rule 2's fixed list beside the controls and tag characters.
func hidden(r rune) bool {
	switch {
	case r == 0x061C, r == 0x200E, r == 0x200F,
		r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
		return true // bidirectional controls
	case r == 0x00AD, r == 0x034F, r == 0x115F, r == 0x1160, r == 0x180E,
		r == 0x200B, r == 0x200C, r >= 0x2060 && r <= 0x2064,
		r == 0x3164, r == 0xFEFF, r == 0xFFA0:
		return true // zero-width and invisible
	case r >= 0xE0100 && r <= 0xE01EF:
		return true // supplementary variation selectors
	}
	return false
}

func control(r rune) bool { return r < 0x20 || r == 0x7F || (r >= 0x80 && r <= 0x9F) }

func tagChar(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }

// removeHidden is rule 2. Its escape-sequence states are termsafe's.
func removeHidden(rs []rune) []rune {
	const (
		text     = iota
		afterEsc // ESC seen, the next character decides the kind
		csi      // to a final byte 0x40 to 0x7E, or a control
		str      // OSC, DCS, SOS, PM, APC: to BEL, U+009C or ESC \
		strEsc   // ESC inside a string
		twoChar  // ESC and intermediates: to a byte 0x30 to 0x7E, or a control
	)
	out := make([]rune, 0, len(rs))
	state := text
	for i := 0; i < len(rs); i++ {
		r := rs[i]
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
			if r >= 0x40 && r <= 0x7E || r < 0x20 {
				state = text
			}
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
			out = append(out, r)
		case r == 0x1B:
			state = afterEsc
		case r == 0x9B:
			state = csi
		case control(r), hidden(r):
		case tagChar(r):
			end := i + 1
			for end < len(rs) && tagChar(rs[end]) {
				end++
			}
			if n := len(out); n > 0 && tbl.tags[string(out[n-1])+string(rs[i:end])] {
				out = append(out, rs[i:end]...)
			}
			i = end - 1
		default:
			out = append(out, r)
		}
	}
	return out
}

// keepSelectors is rule 3.
func keepSelectors(rs []rune) []rune {
	out := rs[:0]
	for _, r := range rs {
		switch {
		case r >= 0xFE00 && r <= 0xFE0D:
			continue
		case r == 0xFE0E:
			if len(out) == 0 || !tbl.text[out[len(out)-1]] {
				continue
			}
		case r == 0xFE0F:
			if len(out) == 0 || !tbl.emoji[out[len(out)-1]] {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// keepJoiners is rule 4: the character before a joiner is read from what
// it has kept, the one after from its input.
func keepJoiners(rs []rune) []rune {
	out := make([]rune, 0, len(rs))
	for i, r := range rs {
		if r == 0x200D {
			k := len(out) - 1
			for k >= 0 && (out[k] == 0xFE0F || modifier(out[k])) {
				k--
			}
			if k < 0 || !pictographic(out[k]) || i+1 >= len(rs) || !pictographic(rs[i+1]) {
				continue
			}
		}
		out = append(out, r)
	}
	return out
}
