package knownkeys

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"
)

func sample(t *testing.T) []Record {
	t.Helper()
	f, err := os.Open("testdata/known_keys.sample")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// The sample is what a second reader of the grammar vendors and parses with
// its own code, so it holds one record of each form among the comment and
// blank lines a hand-edited file has, and every one of them has to parse here.
func TestSampleParses(t *testing.T) {
	recs := sample(t)
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3 (comments and blanks are not records)", len(recs))
	}
	if recs[0].Kind != Active || recs[0].Address != "alice@example.com" || recs[0].Seq != 7 {
		t.Fatalf("active record = %+v", recs[0])
	}
	if recs[1].Kind != RotatedFrom || recs[1].UntilSeq != 6 {
		t.Fatalf("rotated-from record = %+v", recs[1])
	}
	if recs[2].Kind != Retired || recs[2].Address != "bob@example.com" {
		t.Fatalf("retired record = %+v", recs[2])
	}
}

// History must never satisfy a pin. A superseded key that still matches is not
// a rotation, it is a second valid key for the same identity.
func TestRotatedFromIsNeverActive(t *testing.T) {
	recs := sample(t)
	got, ok := ActiveFor(recs, "alice@example.com")
	if !ok {
		t.Fatal("alice has an active pin")
	}
	if got.Kind != Active || got.Seq != 7 {
		t.Fatalf("ActiveFor returned %+v; the superseded key must not match", got)
	}
}

// Retired is a pin that REFUSES, not the absence of one. Collapsing the two
// would let a retired identity be trusted again on first contact.
func TestRetiredIsNotTheSameAsUnknown(t *testing.T) {
	recs := sample(t)
	rec, ok := ActiveFor(recs, "bob@example.com")
	if ok {
		t.Fatal("a retired address has no active pin")
	}
	if rec.Kind != Retired {
		t.Fatalf("a retired address must return its retirement record, got %+v", rec)
	}
	if _, ok := ActiveFor(recs, "nobody@example.com"); ok {
		t.Fatal("an unknown address has no pin")
	}
	if r, _ := ActiveFor(recs, "nobody@example.com"); r.Kind == Retired {
		t.Fatal("an unknown address must not read as retired")
	}
}

func TestFingerprintMatchesTheSample(t *testing.T) {
	recs := sample(t)
	key, err := hex.DecodeString(recs[0].KeyHex)
	if err != nil {
		t.Fatal(err)
	}
	if got := Fingerprint(key); got != recs[0].Fingerprint {
		t.Fatalf("Fingerprint = %s, sample says %s", got, recs[0].Fingerprint)
	}
	if strings.Contains(recs[0].Fingerprint, "=") {
		t.Error("the fingerprint is unpadded base64: padding is what people drop when retyping one")
	}
}

// A malformed line is an error, never a skip. Skipping drops a pin and leaves
// the application trusting first contact again, which is the failure the file
// exists to prevent, arriving silently.
func TestMalformedIsRefusedNotSkipped(t *testing.T) {
	for name, line := range map[string]string{
		"short":        "alice@example.com secp256k1",
		"wrong algo":   "alice@example.com ed25519 02a1b2",
		"short key":    "alice@example.com secp256k1 02a1b2",
		"bad hex":      "alice@example.com secp256k1 " + strings.Repeat("zz", 33),
		"bare token":   "alice@example.com secp256k1 " + k1 + " nonsense",
		"unknown key":  "alice@example.com secp256k1 " + k1 + " future=1",
		"bad sequence": "alice@example.com secp256k1 " + k1 + " seq=notanumber",
		"bad time":     "alice@example.com secp256k1 " + k1 + " first=yesterday",
	} {
		if _, err := Parse(strings.NewReader(line)); err == nil {
			t.Errorf("%s: accepted %q", name, line)
		}
	}
}

// A line wrong in both its algo and its key is refused for the algo: the
// algo says what kind of line it is, and a key's size means nothing until
// that is known.
func TestTheAlgoIsCheckedBeforeTheKey(t *testing.T) {
	_, err := Parse(strings.NewReader("alice@example.com ed25519 02a1b2"))
	if err == nil || !strings.Contains(err.Error(), `algo "ed25519"`) {
		t.Fatalf("the refusal should name the algo: %v", err)
	}
}

// An unknown field is refused rather than ignored, so an old binary cannot
// read a newer file as if the field were absent.
func TestUnknownFieldNamesItself(t *testing.T) {
	_, err := Parse(strings.NewReader("alice@example.com secp256k1 " + k1 + " revoked=true"))
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("the refusal should name the field it did not understand: %v", err)
	}
}

// The field is a COMPRESSED key, which pins the prefix as well as the length.
// A 33-byte blob starting 0x04 is what a truncated uncompressed key looks like
// after a copy-paste, and it fingerprints as cleanly as a real pin does.
func TestKeyPrefixMustBeCompressed(t *testing.T) {
	_, err := Parse(strings.NewReader("alice@example.com secp256k1 04" + strings.Repeat("11", 32)))
	if err == nil {
		t.Fatal("a key whose prefix is neither 0x02 nor 0x03 was accepted")
	}
	if !strings.Contains(err.Error(), "0x02") || !strings.Contains(err.Error(), "0x03") {
		t.Fatalf("the refusal should say what a compressed key starts with: %v", err)
	}
	for _, good := range []string{k1, k2} {
		if _, err := Parse(strings.NewReader("alice@example.com secp256k1 " + good)); err != nil {
			t.Fatalf("a compressed key was refused: %v", err)
		}
	}
}

// Two active pins for one address is the store holding two answers to the one
// question it exists to answer, and ActiveFor would return whichever line came
// first. The refusal has to name both lines or the reader cannot tell which of
// them to delete.
func TestASecondActivePinIsRefused(t *testing.T) {
	file := "# alice, twice\n" +
		"alice@example.com secp256k1 " + k1 + " seq=1\n" +
		"@rotated-from alice@example.com secp256k1 " + k2 + " until_seq=1\n" +
		"alice@example.com secp256k1 " + k2 + " seq=2\n"
	_, err := Parse(strings.NewReader(file))
	if err == nil {
		t.Fatal("two active pins for one address parsed cleanly")
	}
	for _, want := range []string{"alice@example.com", "line 2", "line 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal should name the address and both line numbers, got: %v", err)
		}
	}
	// The same key twice is still two records. The rule is one active record
	// per address, not one distinct answer per address.
	if _, err := Parse(strings.NewReader("a@example.com secp256k1 " + k1 + "\na@example.com secp256k1 " + k1 + "\n")); err == nil {
		t.Fatal("a repeated active line was accepted")
	}
	// History and a retirement are not pins in force, so they do not collide,
	// with each other or with the one active line beside them. That line stays
	// the answer. The retirement comes first on purpose: it is the order Pin
	// leaves when it records a key for an address that holds only a
	// retirement, and the order in which a reading that let the retirement
	// win would turn a verified answer into a refused one.
	both := "@rotated-from a@example.com secp256k1 " + k1 + "\n@rotated-from a@example.com secp256k1 " + k2 + "\n" +
		"@retired a@example.com secp256k1 " + k1 + " at=2026-01-01T00:00:00Z\n" +
		"a@example.com secp256k1 " + k2 + " seq=2\n"
	recs, err := Parse(strings.NewReader(both))
	if err != nil {
		t.Fatalf("history and a retirement beside one active pin are legal: %v", err)
	}
	if got, ok := ActiveFor(recs, "a@example.com"); !ok || got.Kind != Active || got.KeyHex != k2 {
		t.Fatalf("ActiveFor = %+v, %v; the active line is the pin in force beside a retirement", got, ok)
	}
}

// The refusal of a second active pin is pinned whole: the line it stopped at
// comes first and the earlier line second, and a reader deciding which one to
// delete depends on which is which.
func TestASecondActivePinNamesTheLaterLineFirst(t *testing.T) {
	file := "# alice, twice\n" +
		"alice@example.com secp256k1 " + k1 + " seq=1\n" +
		"@rotated-from alice@example.com secp256k1 " + k2 + " until_seq=1\n" +
		"alice@example.com secp256k1 " + k2 + " seq=2\n"
	_, err := Parse(strings.NewReader(file))
	want := "known_keys line 4: alice@example.com already has an active pin at line 2; at most one per address"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want\n%q", err, want)
	}
}

// Line writes every field that is set, in one fixed order whatever the kind,
// and every time in UTC. The order is part of the grammar: two writers that
// ordered the fields differently would make the same store two files.
func TestLineWritesEveryFieldInItsFixedOrder(t *testing.T) {
	rec := Record{
		Address:     "a@example.com",
		Algo:        "secp256k1",
		KeyHex:      k1,
		Seq:         7,
		UntilSeq:    6,
		First:       time.Date(2026, 1, 1, 2, 0, 0, 0, time.FixedZone("UTC+2", 2*60*60)),
		Last:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		At:          time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		Fingerprint: "SHA256:x",
	}
	const fields = " seq=7 until_seq=6 first=2026-01-01T00:00:00Z last=2026-01-02T00:00:00Z at=2026-01-03T00:00:00Z fp=SHA256:x"
	for kind, prefix := range map[Kind]string{Active: "", RotatedFrom: "@rotated-from ", Retired: "@retired "} {
		rec.Kind = kind
		if got, want := rec.Line(), prefix+"a@example.com secp256k1 "+k1+fields; got != want {
			t.Errorf("kind %d:\n%s\nwant\n%s", kind, got, want)
		}
	}
}

// A hand-edited file indents and spaces its lines. A line that is only
// whitespace, and a comment with whitespace before its '#', are skipped like
// any blank or comment rather than read as a record, which would refuse the
// whole store over layout.
func TestBlankAndIndentedCommentLinesAreSkipped(t *testing.T) {
	file := "# example known_keys v1\n" +
		"   \n" +
		"\t\n" +
		"  # an indented comment\n" +
		"\t# a tab-indented comment\n" +
		"alice@example.com secp256k1 " + k1 + " seq=1\n"
	recs, err := Parse(strings.NewReader(file))
	if err != nil {
		t.Fatalf("whitespace-only lines and indented comments were refused: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (blanks and comments are not records)", len(recs))
	}
}

// A line is read whole up to a 1 MiB bound, well past the scanner's default
// of 64 KiB, so a long comment or address does not refuse a store that parses
// line by line. The bound is still a bound: a line past it is an error.
func TestALongLineIsReadWholeUpToTheBound(t *testing.T) {
	long := "# " + strings.Repeat("x", 70*1024) + "\n" +
		"alice@example.com secp256k1 " + k1 + " seq=1\n"
	recs, err := Parse(strings.NewReader(long))
	if err != nil {
		t.Fatalf("a %d-byte comment line was refused: %v", 70*1024+2, err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if _, err := Parse(strings.NewReader("# " + strings.Repeat("x", 1<<20) + "\n")); err == nil {
		t.Fatal("a line past the 1 MiB bound was accepted")
	}
}

// With more than one retirement for an address and no active pin, the one on
// the later line is the answer. Which record answers is part of the grammar a
// second reader has to match, so it is pinned rather than left to the loop.
func TestTheLastRetirementAnswers(t *testing.T) {
	file := "@retired a@example.com secp256k1 " + k1 + " at=2026-01-01T00:00:00Z\n" +
		"@retired a@example.com secp256k1 " + k2 + " at=2026-02-01T00:00:00Z\n"
	recs, err := Parse(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := ActiveFor(recs, "a@example.com")
	if ok {
		t.Fatal("a retired address has no active pin")
	}
	if got.Kind != Retired || got.KeyHex != k2 || !got.At.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("ActiveFor = %+v, want the later retirement", got)
	}
}

// alias is 02 || p+1: go-sdk reads it as the point with x = 1 and writes it
// back unreduced, so as bytes it is a second key for one point.
const alias = "02fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc30"

// A key must be the one canonical encoding of a point on the curve, in the
// file and in what Pin and Rotate would write to it.
func TestKeyMustBeCanonical(t *testing.T) {
	x1 := "02" + strings.Repeat("00", 31) + "01"
	offCurve := "02" + strings.Repeat("00", 31) + "05"
	for _, bad := range []string{alias, offCurve} {
		if _, err := Parse(strings.NewReader("alice@example.com secp256k1 " + bad)); err == nil {
			t.Fatalf("%s was parsed", bad)
		}
		if _, err := Pin(nil, "alice@example.com", bad, 1, "", time.Now()); err == nil {
			t.Fatalf("%s was pinned", bad)
		}
		recs, err := Pin(nil, "alice@example.com", x1, 1, "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Rotate(recs, "alice@example.com", bad, 2, "", time.Now()); err == nil {
			t.Fatalf("%s was rotated to", bad)
		}
	}
	if _, err := Pin(nil, "alice@example.com", strings.ToUpper(k1), 1, "", time.Now()); err == nil {
		t.Fatal("an upper-case key was pinned")
	}
}
