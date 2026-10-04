package sanitize

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lightwebinc/bcommon/termsafe"
)

// corpus is testdata/vectors/sanitize-v1.json, which tools/vectors wrote
// with a second implementation of the rules that reads the Unicode files
// and not the table. The TypeScript tests read the same file.
type corpus struct {
	Unicode     string `json:"unicode"`
	TableSHA256 string `json:"tableSha256"`
	Cases       []struct {
		Name      string `json:"name"`
		InputHex  string `json:"inputHex"`
		OutputHex string `json:"outputHex"`
	} `json:"cases"`
}

func loadCorpus(t *testing.T) *corpus {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "sanitize-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return &c
}

func TestCorpus(t *testing.T) {
	c := loadCorpus(t)
	sum := sha256.Sum256(tableJSON)
	if c.Unicode != UnicodeVersion || c.TableSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("the corpus is pinned to Unicode %s, table %s; this is %s, %x", c.Unicode, c.TableSHA256, UnicodeVersion, sum)
	}
	if len(c.Cases) < 80 {
		t.Fatalf("%d cases", len(c.Cases))
	}
	for _, k := range c.Cases {
		in, err := hex.DecodeString(k.InputHex)
		if err != nil {
			t.Fatal(err)
		}
		got := Filter(string(in))
		if hex.EncodeToString([]byte(got)) != k.OutputHex {
			t.Errorf("%s: %+q, want %s", k.Name, got, k.OutputHex)
		}
		// The rules are a fixed point: the output filters to itself.
		if again := Filter(got); again != got {
			t.Errorf("%s: filtering the output again gives %+q", k.Name, again)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: the output is not UTF-8", k.Name)
		}
	}
}

// The TypeScript package reads the same table: its module holds the
// embedded file's bytes as one string literal.
func TestTypeScriptTableIsTheSameBytes(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "ts", "src", "sanitize-table.ts"))
	if err != nil {
		t.Fatal(err)
	}
	const decl = "export const unicodeTableJSON = "
	_, lit, ok := strings.Cut(string(src), decl)
	if !ok {
		t.Fatalf("no %q in the module", decl)
	}
	var s string
	if err := json.Unmarshal([]byte(strings.TrimSpace(lit)), &s); err != nil {
		t.Fatal(err)
	}
	if s != string(tableJSON) {
		t.Fatal("ts/src/sanitize-table.ts holds other bytes than unicode-15.1.json")
	}
}

// What the table holds that the rules name: the three flags as tag
// sequences, the modifiers, and the bases the named cases rely on.
func TestTable(t *testing.T) {
	if len(tbl.TagSequences) != 3 || len(tbl.Sources) != 3 {
		t.Fatalf("%d tag sequences, %d sources", len(tbl.TagSequences), len(tbl.Sources))
	}
	for _, r := range []rune{0x2764, 0x1F3F3, 0x26A7, 0x1F525, 0x1F308, 0x1F468, 0x1F3F4} {
		if !pictographic(r) {
			t.Errorf("U+%04X is not Extended_Pictographic", r)
		}
	}
	for _, r := range []rune{'a', '1', 0x1F3FB, 0x200D, 0xFE0F, 0xE0067} {
		if pictographic(r) {
			t.Errorf("U+%04X is Extended_Pictographic", r)
		}
	}
	for r := rune(0x1F3FA); r <= 0x1F400; r++ {
		if modifier(r) != (r >= 0x1F3FB && r <= 0x1F3FF) {
			t.Errorf("modifier(U+%04X)", r)
		}
	}
	if !tbl.emoji[0x2764] || !tbl.text[0x2764] || !tbl.emoji['#'] || tbl.emoji[0x1F600] || tbl.emoji['a'] {
		t.Error("variation bases")
	}
}

// Rule 2 removes what termsafe removes as a control, an escape sequence, a
// zero-width or a bidirectional character, so a terminal renderer that
// filters and then calls termsafe.Text sees nothing more removed by
// termsafe on that account: what termsafe still drops is what it does not
// print (the joiner among them).
func TestTermsafeAfterFilter(t *testing.T) {
	for _, s := range []string{"a\x1b]0;t\x07b\x1b[31mc\u202Ed\u200Be\tf\r", "go \u2764\uFE0F\u200D\U0001F525"} {
		f := Filter(s)
		if strings.ContainsAny(f, "\x1b\r\t\u202E\u200B") {
			t.Errorf("%+q: %+q", s, f)
		}
		if got := termsafe.Text(f); strings.ContainsAny(got, "\x1b") {
			t.Errorf("%+q: termsafe gives %+q", s, got)
		}
	}
}

func FuzzFilter(f *testing.F) {
	c := loadCorpusF(f)
	for _, k := range c.Cases {
		in, _ := hex.DecodeString(k.InputHex)
		f.Add(in)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		got := Filter(string(in))
		if !utf8.ValidString(got) {
			t.Fatalf("%+q: not UTF-8", got)
		}
		if Filter(got) != got {
			t.Fatalf("%+q: not a fixed point", got)
		}
		for _, r := range got {
			if r != '\n' && (control(r) || hidden(r)) {
				t.Fatalf("%+q kept U+%04X", got, r)
			}
		}
	})
}

func loadCorpusF(f *testing.F) *corpus {
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "vectors", "sanitize-v1.json"))
	if err != nil {
		f.Fatal(err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		f.Fatal(err)
	}
	return &c
}
