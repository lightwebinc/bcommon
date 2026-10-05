package payee

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The fixtures under testdata/fixtures/payee are an application's own
// ledgers, as its host and its test host wrote them, around four payments
// from one payer on a local chain (offers.json):
//
//   - ledger-v1.jsonl: the first two, as its Go test host writes a ledger
//     (LedgerV1, no inputs and no time);
//   - ledger-v2.jsonl: its host's ledger (LedgerV2): the first two
//     accepted, the first again refused as replayed, the fourth (which
//     spends the first's coin) refused as a conflict, a line cut short by a
//     crash, and after a restart the third accepted;
//   - ledger-mixed.jsonl: its host restarted over ledger-v1.jsonl: the
//     second refused as replayed, the fourth as a conflict with a version 1
//     line (whose coin the host read from its transaction), and the third
//     accepted, appended as LedgerV2;
//   - state.json: its home's state, two payments settled and the fourth
//     recorded as refused.
//
// Only the time each host line carries is changed, to a fixed value.
const fixtures = "../testdata/fixtures/payee"

type offer struct {
	Txid              string `json:"txid"`
	Beef              string `json:"beef"`
	Satoshis          uint64 `json:"satoshis"`
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
}

func offers(t *testing.T) (payee string, offs []offer) {
	t.Helper()
	var d struct {
		Payee    string  `json:"payee"`
		Payments []offer `json:"payments"`
	}
	readJSON(t, "offers.json", &d)
	if len(d.Payments) != 4 {
		t.Fatalf("offers.json holds %d payments, want 4", len(d.Payments))
	}
	return d.Payee, d.Payments
}

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// appLine and appRead are the application's own ledger reader, verbatim but
// for its warning stream: what this package's reader must agree with.
type appLine struct {
	Txid              string `json:"txid"`
	Beef              string `json:"beef"`
	OutputIndex       uint32 `json:"outputIndex"`
	Satoshis          uint64 `json:"satoshis"`
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
	Class             string `json:"class"`
}

func appRead(t *testing.T, path string) (lines []appLine, skipped []int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		n++
		tx := strings.TrimSpace(sc.Text())
		if tx == "" {
			continue
		}
		var l appLine
		if err := json.Unmarshal([]byte(tx), &l); err != nil || l.Txid == "" {
			skipped = append(skipped, n)
			continue
		}
		lines = append(lines, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines, skipped
}

var ledgers = []struct {
	name     string
	versions []int
	skipped  string
}{
	{"ledger-v1.jsonl", []int{1, 1}, ""},
	{"ledger-v2.jsonl", []int{2, 2, 2}, "line 3: not a payment; skipped\n"},
	{"ledger-mixed.jsonl", []int{1, 1, 2}, ""},
}

// Every fixture reads as the application reads it: the same payments, the
// same lines skipped, said in the same words.
func TestLedgerReadsAsTheApplicationReadsIt(t *testing.T) {
	for _, l := range ledgers {
		path := filepath.Join(fixtures, l.name)
		want, wantSkipped := appRead(t, path)
		var warn bytes.Buffer
		got, err := ReadLedgers(&warn, path)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) || len(got) != len(l.versions) {
			t.Fatalf("%s: %d payments, the application reads %d", l.name, len(got), len(want))
		}
		for i, p := range got {
			a := appLine{p.Txid, p.Beef, p.OutputIndex, p.Satoshis, p.DerivationPrefix, p.DerivationSuffix, p.SenderIdentityKey, p.Class}
			if a != want[i] {
				t.Errorf("%s payment %d:\n%+v\nthe application reads\n%+v", l.name, i, a, want[i])
			}
			if p.Version() != l.versions[i] {
				t.Errorf("%s payment %d: version %d, want %d", l.name, i, p.Version(), l.versions[i])
			}
		}
		wantWarn := ""
		for _, n := range wantSkipped {
			wantWarn += path + " line " + strconv.Itoa(n) + ": not a payment; skipped\n"
		}
		if l.skipped != "" && wantWarn != path+" "+l.skipped {
			t.Fatalf("%s: the application skips %q", l.name, wantWarn)
		}
		if warn.String() != wantWarn {
			t.Errorf("%s: warned %q, want %q", l.name, warn.String(), wantWarn)
		}
	}
}

// Line writes every line a host wrote byte for byte, in both versions: the
// Go test host's and the TypeScript host's.
func TestLineIsTheHostsBytes(t *testing.T) {
	for _, l := range ledgers {
		raw, err := os.ReadFile(filepath.Join(fixtures, l.name))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for line := range strings.SplitAfterSeq(string(raw), "\n") {
			p, err := ParseLine([]byte(line))
			if err != nil {
				continue
			}
			n++
			got, err := p.Line()
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != line {
				t.Errorf("%s: Line wrote\n%s\nthe host wrote\n%s", l.name, got, line)
			}
		}
		if n != len(l.versions) {
			t.Fatalf("%s: compared %d lines, want %d", l.name, n, len(l.versions))
		}
	}
}

// A version 1 line's coins, read from its transaction, are the coins the
// host recorded for the same payment in a version 2 line.
func TestSpendsOfAVersion1LineAreTheHosts(t *testing.T) {
	v1, err := ReadLedgers(nil, filepath.Join(fixtures, "ledger-v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := ReadLedgers(nil, filepath.Join(fixtures, "ledger-v2.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range v1 {
		if v1[i].Inputs != nil || v1[i].Txid != v2[i].Txid {
			t.Fatalf("fixture order: %s %s", v1[i].Txid, v2[i].Txid)
		}
		got, err := v1[i].Spends()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, v2[i].Inputs) || len(got) == 0 {
			t.Errorf("payment %s spends %v, the host recorded %v", v1[i].Txid, got, v2[i].Inputs)
		}
		own, _ := v2[i].Spends()
		if !reflect.DeepEqual(own, v2[i].Inputs) {
			t.Errorf("a version 2 line's own inputs: %v", own)
		}
	}
	bad := Payment{Txid: "x", Beef: "not base64!"}
	if _, err := bad.Spends(); err == nil {
		t.Error("a BEEF that is not base64 has coins")
	}
	bad.Beef = "AAAA"
	if _, err := bad.Spends(); err == nil {
		t.Error("bytes that are not a BEEF have coins")
	}
}

// Claims answers as the host answered over the same ledgers: what it
// refused is not in the file, and what it accepted is.
func TestClaimsAnswerAsTheHost(t *testing.T) {
	_, offs := offers(t)
	spends := func(o offer) []string {
		in, err := Payment{Txid: o.Txid, Beef: o.Beef}.Spends()
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	read := func(name string) []Payment {
		ps, err := ReadLedgers(nil, filepath.Join(fixtures, name))
		if err != nil {
			t.Fatal(err)
		}
		return ps
	}
	// The host restarted over the version 1 ledger.
	c := ClaimsOf(read("ledger-v1.jsonl"))
	for _, x := range []struct {
		o    offer
		want Claim
	}{{offs[1], Replayed}, {offs[3], Conflict}, {offs[2], Accepted}} {
		if got := c.Claim(x.o.Txid, spends(x.o)); got != x.want {
			t.Errorf("over ledger-v1: %s is %v, the host answered %v", x.o.Txid, got, x.want)
		}
	}
	mixed := read("ledger-mixed.jsonl")
	if len(mixed) != 3 || mixed[2].Txid != offs[2].Txid {
		t.Fatal("ledger-mixed holds what the host accepted, and only that")
	}
	// The host that wrote the version 2 ledger, after its restart.
	c = ClaimsOf(read("ledger-v2.jsonl"))
	if got := c.Refusal(offs[0].Txid, spends(offs[0])); got != Replayed {
		t.Errorf("the first again: %v", got)
	}
	if got := c.Refusal(offs[3].Txid, spends(offs[3])); got != Conflict {
		t.Errorf("the first's coin again: %v", got)
	}
	if got := c.Refusal("ab", nil); got != Conflict {
		t.Errorf("a payment that names no coin: %v", got)
	}
	var zero Claims
	if got := zero.Claim("ab", []string{"cd.0"}); got != Accepted || zero.Refusal("ab", []string{"ef.0"}) != Replayed {
		t.Errorf("the zero value: %v", got)
	}
	if Accepted.String() != "accepted" || Replayed.String() != "replayed" || Conflict.String() != "conflict" || Claim(9).String() != "claim(9)" {
		t.Error("claim names")
	}
}

// A line of a later version is read, and named by its version.
func TestALaterVersionIsReadAndNamed(t *testing.T) {
	p, err := ParseLine([]byte(`{"v":3,"txid":"ab","beef":"","outputIndex":0,"satoshis":1,"derivationPrefix":"","derivationSuffix":"","senderIdentityKey":"","class":"c","more":true}`))
	if err != nil || p.Version() != 3 {
		t.Fatalf("%v %d", err, p.Version())
	}
	for _, s := range []string{`{"txid":""}`, `{"txid":"ab"`, `[1]`, `{"txid":"ab","outputIndex":-1}`} {
		if _, err := ParseLine([]byte(s)); err != ErrNotPayment {
			t.Errorf("%s: %v", s, err)
		}
	}
}

// Book in an application's state keeps the state file's bytes.
func TestBookKeepsTheStateFileBytes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtures, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Version  int    `json:"version"`
		Identity string `json:"identity"`
		Book
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(append(out, '\n')) != string(raw) {
		t.Fatalf("written back:\n%s\nthe application wrote:\n%s", out, raw)
	}
	_, offs := offers(t)
	if !st.IsSettled(offs[0].Txid) || !st.IsSettled(offs[2].Txid) || st.IsSettled(offs[1].Txid) || !st.IsUnsettleable(offs[3].Txid) || st.IsUnsettleable(offs[0].Txid) {
		t.Fatal("the record read back")
	}
	st.AddSettled(offs[0].Txid)
	st.AddUnsettleable(offs[3].Txid, "again")
	if len(st.Settled) != 2 || len(st.Unsettleable) != 1 {
		t.Fatal("a payment recorded twice")
	}
	saves := 0
	r := Saved(&st.Book, func() error { saves++; return nil })
	if err := r.RecordSettled(offs[1].Txid); err != nil || !r.IsSettled(offs[1].Txid) || saves != 1 {
		t.Fatal("Saved records and saves")
	}
	if err := r.RecordUnsettleable("cd", "why"); err != nil || !r.IsUnsettleable("cd") || saves != 2 {
		t.Fatal("Saved records a refusal and saves")
	}
}
