package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The Unicode emoji data the renderer filter's property table is generated
// from, vendored under third_party/unicode/<version> with Unicode's licence.
const (
	unicodeVersion = "15.1"
	unicodeDir     = "third_party/unicode/15.1"
)

var unicodeSources = []string{"emoji-data.txt", "emoji-variation-sequences.txt", "emoji-sequences.txt"}

// The table's two outputs: the file the Go package embeds, and the
// TypeScript module that holds the same bytes as one string literal.
const (
	tableGo = "sanitize/unicode-15.1.json"
	tableTS = "ts/src/sanitize-table.ts"
)

// emojiData is what the filter reads from the three files.
type emojiData struct {
	sha          map[string]string
	pictographic [][2]rune // Extended_Pictographic, merged ranges
	modifier     [][2]rune // Emoji_Modifier
	textVS       []rune    // bases listed with U+FE0E
	emojiVS      []rune    // bases listed with U+FE0F
	tagSeqs      [][]rune  // RGI_Emoji_Tag_Sequence, whole
}

func readUnicode(repo string) (*emojiData, error) {
	d := &emojiData{sha: map[string]string{}}
	raw := map[string][]byte{}
	for _, f := range unicodeSources {
		b, err := os.ReadFile(filepath.Join(repo, unicodeDir, f))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		d.sha[f] = hex.EncodeToString(sum[:])
		raw[f] = b
	}
	if !bytes.Contains(raw["emoji-data.txt"], []byte("Used with Emoji Version "+unicodeVersion+" ")) ||
		!bytes.Contains(raw["emoji-sequences.txt"], []byte("# Version: "+unicodeVersion+"\n")) {
		return nil, fmt.Errorf("%s does not hold Unicode %s's emoji data", unicodeDir, unicodeVersion)
	}
	err := eachLine(raw["emoji-data.txt"], func(fields []string) error {
		lo, hi, err := cpRange(fields[0])
		if err != nil {
			return err
		}
		switch fields[1] {
		case "Extended_Pictographic":
			d.pictographic = append(d.pictographic, [2]rune{lo, hi})
		case "Emoji_Modifier":
			d.modifier = append(d.modifier, [2]rune{lo, hi})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("emoji-data.txt: %w", err)
	}
	err = eachLine(raw["emoji-variation-sequences.txt"], func(fields []string) error {
		cps, err := cpList(fields[0])
		if err != nil {
			return err
		}
		if len(cps) != 2 {
			return fmt.Errorf("%q is not a base and a selector", fields[0])
		}
		switch {
		case cps[1] == 0xFE0E && fields[1] == "text style":
			d.textVS = append(d.textVS, cps[0])
		case cps[1] == 0xFE0F && fields[1] == "emoji style":
			d.emojiVS = append(d.emojiVS, cps[0])
		default:
			return fmt.Errorf("%q: %q", fields[0], fields[1])
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("emoji-variation-sequences.txt: %w", err)
	}
	err = eachLine(raw["emoji-sequences.txt"], func(fields []string) error {
		if fields[1] != "RGI_Emoji_Tag_Sequence" {
			return nil
		}
		cps, err := cpList(fields[0])
		if err != nil {
			return err
		}
		d.tagSeqs = append(d.tagSeqs, cps)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("emoji-sequences.txt: %w", err)
	}
	d.pictographic, d.modifier = merge(d.pictographic), merge(d.modifier)
	for _, s := range [][]rune{d.textVS, d.emojiVS} {
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	}
	sort.Slice(d.tagSeqs, func(i, j int) bool { return lessRunes(d.tagSeqs[i], d.tagSeqs[j]) })

	// What the filter's rules name, held to the data: the modifiers are
	// U+1F3FB to U+1F3FF, the three flags are the only tag sequences, and
	// every tag sequence is a base then tag characters ending U+E007F.
	if len(d.modifier) != 1 || d.modifier[0] != [2]rune{0x1F3FB, 0x1F3FF} {
		return nil, fmt.Errorf("Emoji_Modifier is %v", d.modifier)
	}
	if len(d.tagSeqs) != 3 {
		return nil, fmt.Errorf("%d tag sequences", len(d.tagSeqs))
	}
	for _, s := range d.tagSeqs {
		if len(s) < 3 || s[len(s)-1] != 0xE007F || isTag(s[0]) {
			return nil, fmt.Errorf("tag sequence %X", s)
		}
		for _, r := range s[1:] {
			if !isTag(r) {
				return nil, fmt.Errorf("tag sequence %X", s)
			}
		}
	}
	if len(d.textVS) != len(d.emojiVS) || len(d.pictographic) < 50 {
		return nil, fmt.Errorf("%d text and %d emoji variation sequences, %d pictographic ranges", len(d.textVS), len(d.emojiVS), len(d.pictographic))
	}
	return d, nil
}

// eachLine calls f with the semicolon-separated fields of every data line,
// comments and blank lines skipped, each field trimmed.
func eachLine(b []byte, f func([]string) error) error {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ";")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) < 2 {
			return fmt.Errorf("line %q", sc.Text())
		}
		if err := f(fields); err != nil {
			return err
		}
	}
	return sc.Err()
}

func cp(s string) (rune, error) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil || v > 0x10FFFF {
		return 0, fmt.Errorf("code point %q", s)
	}
	return rune(v), nil
}

func cpRange(s string) (rune, rune, error) {
	a, b, ok := strings.Cut(s, "..")
	lo, err := cp(a)
	if err != nil {
		return 0, 0, err
	}
	if !ok {
		return lo, lo, nil
	}
	hi, err := cp(b)
	if err != nil || hi < lo {
		return 0, 0, fmt.Errorf("range %q", s)
	}
	return lo, hi, nil
}

func cpList(s string) ([]rune, error) {
	var out []rune
	for _, f := range strings.Fields(s) {
		r, err := cp(f)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func merge(rs [][2]rune) [][2]rune {
	sort.Slice(rs, func(i, j int) bool { return rs[i][0] < rs[j][0] })
	var out [][2]rune
	for _, r := range rs {
		if n := len(out); n > 0 && r[0] <= out[n-1][1]+1 {
			out[n-1][1] = max(out[n-1][1], r[1])
			continue
		}
		out = append(out, r)
	}
	return out
}

func lessRunes(a, b []rune) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func isTag(r rune) bool { return r >= 0xE0000 && r <= 0xE007F }

// table renders the property table: one JSON object, one member a line,
// so that a change of Unicode version is a readable diff.
func (d *emojiData) table() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{\n")
	members := []struct {
		name string
		v    any
	}{
		{"unicode", unicodeVersion},
		{"sources", d.sha},
		{"extendedPictographic", d.pictographic},
		{"emojiModifier", d.modifier},
		{"textVariationBases", d.textVS},
		{"emojiVariationBases", d.emojiVS},
		{"tagSequences", d.tagSeqs},
	}
	for i, m := range members {
		v, err := json.Marshal(m.v)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "  %q: %s", m.name, v)
		if i < len(members)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

// tableModule is the TypeScript module that carries the table's bytes.
func tableModule(table []byte) ([]byte, error) {
	lit, err := json.Marshal(string(table))
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString("// GENERATED by tools/vectors from " + unicodeDir + ". Do not edit.\n")
	b.WriteString("//\n")
	b.WriteString("// The renderer filter's property table: the same bytes as " + tableGo + ",\n")
	b.WriteString("// which the Go package embeds, as one string literal, so that the two\n")
	b.WriteString("// languages read one table. make vectors checks that the two agree.\n")
	b.WriteString("export const unicodeTableJSON = ")
	b.Write(lit)
	b.WriteString("\n")
	return b.Bytes(), nil
}

// tableFiles are the generated files that live outside testdata/vectors,
// by their path from the repository root.
func tableFiles(repo string) (map[string][]byte, *emojiData, error) {
	d, err := readUnicode(repo)
	if err != nil {
		return nil, nil, err
	}
	t, err := d.table()
	if err != nil {
		return nil, nil, err
	}
	m, err := tableModule(t)
	if err != nil {
		return nil, nil, err
	}
	return map[string][]byte{tableGo: t, tableTS: m}, d, nil
}
