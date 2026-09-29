package publish

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	txA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	txB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	txC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestJournalRoundTripAndOrdering(t *testing.T) {
	j := Journal{Dir: filepath.Join(t.TempDir(), "journal")}
	if got, err := j.List(); err != nil || len(got) != 0 {
		t.Fatalf("empty journal: %v %v", got, err)
	}
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	// Written out of order; List must sort by sequence, numerically, so
	// seq 10 does not land between 1 and 2.
	for _, e := range []Entry{
		{Acct: "alice@example.com", IdentityKey: "02aa", Seq: 10, Kind: "update", TxID: txB, CarrierTxID: txC},
		{Acct: "alice@example.com", IdentityKey: "02aa", Seq: 2, Kind: "update", TxID: txC},
		{Acct: "alice@example.com", IdentityKey: "02aa", Seq: 1, Kind: "create", TxID: txA, EFSentAt: &now, BEEFSteak: json.RawMessage(steakWrapped)},
	} {
		if err := j.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	if got := mode(t, j.path(1, txA)); got != 0o600 {
		t.Fatalf("entry mode %o, want 0600", got)
	}
	if got := mode(t, j.Dir); got != 0o700 {
		t.Fatalf("journal dir mode %o, want 0700", got)
	}
	if left, _ := filepath.Glob(filepath.Join(j.Dir, ".*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	got, err := j.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Seq != 1 || got[1].Seq != 2 || got[2].Seq != 10 {
		t.Fatalf("order: %v", seqs(got))
	}
	// The STEAK is compared compacted: encoding/json re-indents a
	// RawMessage on the way out, and what matters is that the same JSON
	// value came back, not the same whitespace.
	if got[0].EFSentAt == nil || !got[0].EFSentAt.Equal(now) || compact(t, got[0].BEEFSteak) != compact(t, []byte(steakWrapped)) || got[0].Kind != "create" {
		t.Fatalf("entry 1 did not round-trip: %+v", got[0])
	}
	if got[2].CarrierTxID != txC {
		t.Fatalf("carrier txid lost: %+v", got[2])
	}
	// The file is named <seq>-<txid>.json.
	if _, err := os.Stat(filepath.Join(j.Dir, "10-"+txB+".json")); err != nil {
		t.Fatal(err)
	}
}

// The journal is read back by whatever an operator runs against it, from this
// release or an older one, so its bytes are frozen: the file name, every tag,
// MarshalIndent's two-space layout, the RFC 3339 times, which fields are left
// out when empty, and no trailing newline. The round-trip tests cannot see a
// renamed tag, because the renamed tag reads back through the same struct.
func TestJournalEntryBytesAreFrozen(t *testing.T) {
	efAt := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	beefAt := time.Date(2026, 9, 24, 12, 0, 1, 500000000, time.UTC)
	minedAt := time.Date(2026, 9, 24, 12, 10, 0, 0, time.UTC)
	for _, row := range []struct {
		name  string
		entry Entry
		file  string
		want  string
	}{
		{"every field", Entry{
			Acct: "someone@example.com", IdentityKey: "02abcd", Seq: 12, Kind: "update", TxID: txA, CarrierTxID: txC,
			EFSentAt: &efAt, EFError: "ingress refused", BEEFSentAt: &beefAt,
			BEEFSteak: json.RawMessage(`{"STEAK":{"tm_example":{"outputsToAdmit":[0],"coinsToRetain":[]}}}`),
			BEEFError: "facade answered 502", MinedAt: &minedAt, Height: 1200,
		}, "12-" + txA + ".json", `{
  "acct": "someone@example.com",
  "identityKey": "02abcd",
  "seq": 12,
  "kind": "update",
  "txid": "` + txA + `",
  "carrierTxid": "` + txC + `",
  "efSentAt": "2026-09-24T12:00:00Z",
  "efError": "ingress refused",
  "beefSentAt": "2026-09-24T12:00:01.5Z",
  "beefSteak": {
    "STEAK": {
      "tm_example": {
        "outputsToAdmit": [
          0
        ],
        "coinsToRetain": []
      }
    }
  },
  "beefError": "facade answered 502",
  "minedAt": "2026-09-24T12:10:00Z",
  "height": 1200
}`},
		{"only the required fields", Entry{
			Acct: "someone@example.com", IdentityKey: "02abcd", Seq: 18446744073709551615, Kind: "create", TxID: txB,
		}, "18446744073709551615-" + txB + ".json", `{
  "acct": "someone@example.com",
  "identityKey": "02abcd",
  "seq": 18446744073709551615,
  "kind": "create",
  "txid": "` + txB + `"
}`},
		// The required fields are written even when empty; only the leg
		// stamps are left out.
		{"required fields empty", Entry{TxID: txC}, "0-" + txC + ".json", `{
  "acct": "",
  "identityKey": "",
  "seq": 0,
  "kind": "",
  "txid": "` + txC + `"
}`},
	} {
		j := Journal{Dir: t.TempDir()}
		if err := j.Write(row.entry); err != nil {
			t.Fatalf("%s: %v", row.name, err)
		}
		names, err := filepath.Glob(filepath.Join(j.Dir, "*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(names) != 1 || filepath.Base(names[0]) != row.file {
			t.Fatalf("%s: files %v, want exactly %s", row.name, names, row.file)
		}
		got, err := os.ReadFile(names[0])
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != row.want {
			t.Errorf("%s: journal bytes\n%s\nwant\n%s", row.name, got, row.want)
		}
	}
}

func TestJournalUpdate(t *testing.T) {
	j := Journal{Dir: t.TempDir()}
	if err := j.Write(Entry{Seq: 3, TxID: txA, Kind: "update"}); err != nil {
		t.Fatal(err)
	}
	minedAt := time.Now().UTC().Truncate(time.Second)
	err := j.Update(3, txA, func(e *Entry) {
		e.BEEFError = "facade 502"
		e.MinedAt = &minedAt
		e.Height = 1200
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := j.Read(3, txA)
	if err != nil {
		t.Fatal(err)
	}
	if e.BEEFError != "facade 502" || e.Height != 1200 || e.MinedAt == nil || !e.MinedAt.Equal(minedAt) || e.Kind != "update" {
		t.Fatalf("update lost fields: %+v", e)
	}
	if err := j.Update(4, txA, func(*Entry) {}); !errors.Is(err, ErrNoEntry) {
		t.Fatalf("update of a missing entry: %v, want ErrNoEntry", err)
	}
	if err := j.Update(3, txA, func(e *Entry) { e.Seq = 9 }); err == nil {
		t.Fatal("an update that renames the entry must be refused")
	}
}

func TestJournalRefusesBadNamesAndUnreadableEntries(t *testing.T) {
	j := Journal{Dir: t.TempDir()}
	for _, bad := range []string{"", "abc", "../../etc/passwd", strings.Repeat("zz", 32)} {
		if err := j.Write(Entry{Seq: 1, TxID: bad}); err == nil {
			t.Errorf("txid %q accepted as a file name", bad)
		}
	}
	if err := j.Write(Entry{Seq: 1, TxID: txA}); err != nil {
		t.Fatal(err)
	}
	// A stray file that is not an entry is an error for List, not a skip.
	if err := os.WriteFile(filepath.Join(j.Dir, "notes.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.List(); err == nil {
		t.Fatal("a file that is not <seq>-<txid>.json must fail List")
	}
	if err := os.Remove(filepath.Join(j.Dir, "notes.json")); err != nil {
		t.Fatal(err)
	}
	// An entry whose name disagrees with its content is an error too.
	if err := os.WriteFile(filepath.Join(j.Dir, "5-"+txB+".json"), []byte(`{"seq":6,"txid":"`+txB+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.List(); err == nil || !strings.Contains(err.Error(), "names seq 5") {
		t.Fatalf("mismatched entry: %v", err)
	}
}

// A directory, a dotfile and a name without the .json suffix are not entries,
// so List passes over them. Each stray is shaped to fail every rule but the
// one that skips it: the directory carries an entry's name and suffix, and
// the other two hold bytes that would not parse as an entry.
func TestListPassesOverNonEntries(t *testing.T) {
	j := Journal{Dir: t.TempDir()}
	for _, e := range []Entry{{Seq: 1, TxID: txA}, {Seq: 2, TxID: txB}} {
		if err := j.Write(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(j.Dir, "7-"+txA+".json"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".hidden.json", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(j.Dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := j.List()
	if err != nil {
		t.Fatalf("List over strays: %v", err)
	}
	if len(got) != 2 || got[0].Seq != 1 || got[0].TxID != txA || got[1].Seq != 2 || got[1].TxID != txB {
		t.Fatalf("List over strays: %+v", got)
	}
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func seqs(es []Entry) []uint64 {
	out := make([]uint64, len(es))
	for i, e := range es {
		out[i] = e.Seq
	}
	return out
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}
