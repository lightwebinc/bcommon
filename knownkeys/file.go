// Package knownkeys reads and writes a pin store: the key each address was
// last seen with, one line per record.
//
// The pin is what a signature check cannot give. Every other check asks "is
// this signed by the key it names"; the pin asks "is that the key I saw last
// time". Without it a host that can swap the key it answers with can
// impersonate anyone it serves, and every signature check faithfully verifies
// the impostor.
//
// The grammar is defined HERE and nowhere else. A pin file is meant to have
// more than one reader, and two readers of an unspecified format is how a pin
// silently fails open, so a second reader should vendor a sample's bytes and
// parse them with its own code. testdata/known_keys.sample is this package's
// sample, one record of each form. Where the file lives and the header line
// that opens it belong to the application: Save takes the header, and this
// package has no default path.
package knownkeys

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Kind distinguishes the three record forms.
type Kind int

const (
	// Active is the pin in force. At most one per address.
	Active Kind = iota
	// RotatedFrom is history. It is NEVER matched for verification: a
	// superseded key that could still satisfy a pin is not a rotation, it is
	// a second valid key.
	RotatedFrom
	// Retired is terminal. Any later answer for the address is refused.
	Retired
)

// Record is one line.
type Record struct {
	Kind     Kind
	Address  string
	Algo     string
	KeyHex   string
	Seq      uint64
	UntilSeq uint64
	First    time.Time
	Last     time.Time
	At       time.Time
	// Fingerprint is the line's fp= value as written, for a person to
	// compare by eye. Parse does not check it against KeyHex.
	Fingerprint string
	Raw         string
}

// Fingerprint is SHA256: plus unpadded base64 over the 33-byte compressed key.
//
// Unpadded on purpose: it is read aloud and compared by eye at first contact,
// and trailing '=' is the character people drop when they retype one.
func Fingerprint(compressed []byte) string {
	sum := sha256.Sum256(compressed)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// Parse reads a known_keys file.
//
// A malformed line is an ERROR, not a skip. Skipping one would drop a pin and
// leave the application trusting first contact again for that address, which
// is the failure this file exists to prevent, arriving silently.
//
// A second active record for an address is malformed in the same sense: the
// file would hold two answers to the one question it exists to answer, and
// ActiveFor would return whichever line came first.
func Parse(r io.Reader) ([]Record, error) {
	var out []Record
	activeAt := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Text()
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		rec, err := parseLine(t)
		if err != nil {
			return nil, fmt.Errorf("known_keys line %d: %w", line, err)
		}
		if rec.Kind == Active {
			// Both line numbers, because the reader has to decide which of the
			// two to delete and cannot do that from one of them.
			if prev, dup := activeAt[rec.Address]; dup {
				return nil, fmt.Errorf("known_keys line %d: %s already has an active pin at line %d; at most one per address",
					line, rec.Address, prev)
			}
			activeAt[rec.Address] = line
		}
		rec.Raw = raw
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func parseLine(t string) (Record, error) {
	f := strings.Fields(t)
	var r Record
	switch {
	case f[0] == "@rotated-from":
		r.Kind, f = RotatedFrom, f[1:]
	case f[0] == "@retired":
		r.Kind, f = Retired, f[1:]
	default:
		r.Kind = Active
	}
	if len(f) < 3 {
		return r, fmt.Errorf("want at least <address> <algo> <key>, got %d field(s)", len(f))
	}
	r.Address, r.Algo, r.KeyHex = f[0], f[1], f[2]
	if r.Algo != "secp256k1" {
		return r, fmt.Errorf("algo %q is not secp256k1", r.Algo)
	}
	key, err := hex.DecodeString(r.KeyHex)
	if err != nil || len(key) != 33 {
		return r, fmt.Errorf("key must be 33 bytes of hex (compressed), got %q", r.KeyHex)
	}
	// Compressed pins the prefix as well as the length. 33 bytes starting 0x04
	// is an uncompressed key someone pasted half of, and it fingerprints as
	// cleanly as a real pin, so the length check alone lets a typo become the
	// thing every later answer is compared against.
	if key[0] != 0x02 && key[0] != 0x03 {
		return r, fmt.Errorf("key prefix 0x%02x: a compressed key starts 0x02 or 0x03, got %q", key[0], r.KeyHex)
	}
	for _, kv := range f[3:] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return r, fmt.Errorf("trailing token %q is not key=value", kv)
		}
		switch k {
		case "seq":
			if r.Seq, err = strconv.ParseUint(v, 10, 64); err != nil {
				return r, fmt.Errorf("seq %q: %w", v, err)
			}
		case "until_seq":
			if r.UntilSeq, err = strconv.ParseUint(v, 10, 64); err != nil {
				return r, fmt.Errorf("until_seq %q: %w", v, err)
			}
		case "first":
			if r.First, err = time.Parse(time.RFC3339, v); err != nil {
				return r, fmt.Errorf("first %q: %w", v, err)
			}
		case "last":
			if r.Last, err = time.Parse(time.RFC3339, v); err != nil {
				return r, fmt.Errorf("last %q: %w", v, err)
			}
		case "at":
			if r.At, err = time.Parse(time.RFC3339, v); err != nil {
				return r, fmt.Errorf("at %q: %w", v, err)
			}
		case "fp":
			r.Fingerprint = v
		default:
			// Unknown keys are REFUSED, not ignored. A future field that an
			// old binary silently drops is a pin that means something
			// different to each reader of the same file.
			return r, fmt.Errorf("unknown field %q; this binary does not understand it and will not guess", k)
		}
	}
	return r, nil
}

// Line renders a record in the grammar Parse reads. Only fields that are set
// are written, and only fields Parse understands, so what this writes, Parse
// and any second reader of the grammar accept.
func (r Record) Line() string {
	var f []string
	switch r.Kind {
	case RotatedFrom:
		f = append(f, "@rotated-from")
	case Retired:
		f = append(f, "@retired")
	}
	f = append(f, r.Address, "secp256k1", r.KeyHex)
	if r.Seq != 0 {
		f = append(f, fmt.Sprintf("seq=%d", r.Seq))
	}
	if r.UntilSeq != 0 {
		f = append(f, fmt.Sprintf("until_seq=%d", r.UntilSeq))
	}
	if !r.First.IsZero() {
		f = append(f, "first="+r.First.UTC().Format(time.RFC3339))
	}
	if !r.Last.IsZero() {
		f = append(f, "last="+r.Last.UTC().Format(time.RFC3339))
	}
	if !r.At.IsZero() {
		f = append(f, "at="+r.At.UTC().Format(time.RFC3339))
	}
	if r.Fingerprint != "" {
		f = append(f, "fp="+r.Fingerprint)
	}
	return strings.Join(f, " ")
}

// ActiveFor returns the pin in force for an address.
//
// A retired address returns its retirement record and ok=false: retired is not
// "no pin", it is a pin that refuses, and collapsing the two would let a
// retired identity be re-trusted on first contact.
func ActiveFor(recs []Record, address string) (Record, bool) {
	var retired Record
	var haveRetired bool
	for _, r := range recs {
		if r.Address != address {
			continue
		}
		switch r.Kind {
		case Active:
			return r, true
		case Retired:
			retired, haveRetired = r, true
		}
	}
	if haveRetired {
		return retired, false
	}
	return Record{}, false
}
