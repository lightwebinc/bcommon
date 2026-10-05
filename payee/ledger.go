package payee

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/termsafe"
)

// LedgerFile is the ledger's name in a host's state directory.
const LedgerFile = "payments.jsonl"

// The ledger's versions. A line carries no version of its own up to
// LedgerV2, and is told apart by its shape; a later version writes its
// number in the "v" key.
const (
	// LedgerV1 is txid, beef, outputIndex, satoshis, derivationPrefix,
	// derivationSuffix, senderIdentityKey and class.
	LedgerV1 = 1
	// LedgerV2 adds inputs (every outpoint the payment spends) and at
	// (when it was accepted, Unix seconds).
	LedgerV2 = 2
	// LedgerVersion is the latest version this package reads and the one
	// Payment.Line writes.
	LedgerVersion = LedgerV2
)

// MaxLine is the longest ledger line read: a payment rides in one line, its
// Atomic BEEF in base64.
const MaxLine = 16 << 20

// Payment is one line of a host's ledger: a BRC-105 payment the host
// accepted. Its JSON is the line, in the order a host writes it.
type Payment struct {
	// Txid is the payment's txid, in display order.
	Txid string `json:"txid"`
	// Beef is the payment as Atomic BEEF, standard base64.
	Beef string `json:"beef"`
	// OutputIndex is the output that pays the payee.
	OutputIndex uint32 `json:"outputIndex"`
	// Satoshis is what that output holds.
	Satoshis uint64 `json:"satoshis"`
	// DerivationPrefix and DerivationSuffix are the BRC-29 remittance,
	// base64, and SenderIdentityKey the payer, compressed hex.
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
	// Class is the class of the question it paid for.
	Class string `json:"class"`
	// Inputs are every outpoint the payment spends, "<txid>.<index>" with
	// the txid in display order (LedgerV2).
	Inputs []string `json:"inputs,omitempty"`
	// At is when the host accepted it, Unix seconds (LedgerV2).
	At int64 `json:"at,omitempty"`
	// V is the version a line names, from the first version that names
	// one; zero on every LedgerV1 and LedgerV2 line.
	V int `json:"v,omitempty"`
}

// Version is the ledger version the line was written in.
func (p Payment) Version() int {
	switch {
	case p.V != 0:
		return p.V
	case p.Inputs != nil || p.At != 0:
		return LedgerV2
	}
	return LedgerV1
}

// Spends are the outpoints the payment spends: the line's own (LedgerV2),
// or else read from its transaction, as a host reads a line written before
// it recorded them.
func (p Payment) Spends() ([]string, error) {
	if p.Inputs != nil {
		return slices.Clone(p.Inputs), nil
	}
	raw, err := base64.StdEncoding.DecodeString(p.Beef)
	if err != nil {
		return nil, fmt.Errorf("payee: the ledger's BEEF is not base64: %w", err)
	}
	_, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil || tx == nil {
		return nil, fmt.Errorf("payee: the ledger's BEEF holds no transaction: %v", err)
	}
	out := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		id := ""
		if in.SourceTXID != nil {
			id = in.SourceTXID.String()
		}
		out = append(out, id+"."+strconv.FormatUint(uint64(in.SourceTxOutIndex), 10))
	}
	return out, nil
}

// Line is the payment as a ledger line, newline included, byte for byte as
// a host writes it: JSON with no HTML escaping, keys in the order above.
func (p Payment) Line() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ErrNotPayment is a ledger line that is not a payment.
var ErrNotPayment = errors.New("payee: not a payment")

// ParseLine reads one ledger line. A line that is not JSON, or names no
// txid, is ErrNotPayment.
func ParseLine(line []byte) (Payment, error) {
	var p Payment
	if err := json.Unmarshal(line, &p); err != nil || p.Txid == "" {
		return Payment{}, ErrNotPayment
	}
	return p, nil
}

// ReadLedger reads a ledger. A line that does not parse (one cut short by a
// crash, whose question was never answered) is passed over and named on
// warn, as "<name> line <n>: not a payment; skipped"; warn may be nil.
func ReadLedger(r io.Reader, name string, warn io.Writer) ([]Payment, error) {
	var out []Payment
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), MaxLine)
	n := 0
	for sc.Scan() {
		n++
		t := strings.TrimSpace(sc.Text())
		if t == "" {
			continue
		}
		p, err := ParseLine([]byte(t))
		if err != nil {
			say(warn, fmt.Sprintf("%s line %d: not a payment; skipped", name, n))
			continue
		}
		out = append(out, p)
	}
	return out, sc.Err()
}

// ReadLedgers reads every ledger at paths in turn. A payment in two (a
// copy, or two hosts sharing one) is returned once, as first read.
func ReadLedgers(warn io.Writer, paths ...string) ([]Payment, error) {
	var out []Payment
	seen := map[string]bool{}
	for _, path := range paths {
		ps, err := readLedgerFile(path, warn)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			if !seen[p.Txid] {
				seen[p.Txid] = true
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func readLedgerFile(path string, warn io.Writer) ([]Payment, error) {
	f, err := os.Open(path) //nolint:gosec // the operator's own path
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadLedger(f, path, warn)
}

// Claim is what a host says of a payment offered to it.
type Claim int

const (
	// Accepted is a payment taken.
	Accepted Claim = iota
	// Replayed is a txid accepted before: one payment buys one question.
	Replayed
	// Conflict is a payment that spends a coin an accepted payment spent,
	// or names no coin at all: at most one of two such payments can ever
	// settle, whatever their txids.
	Conflict
)

func (c Claim) String() string {
	switch c {
	case Accepted:
		return "accepted"
	case Replayed:
		return "replayed"
	case Conflict:
		return "conflict"
	}
	return "claim(" + strconv.Itoa(int(c)) + ")"
}

// Claims are the payments a host took, by txid and by the coins they spend.
// A payment is unbroadcast when it is offered, so two transactions spending
// one coin both verify. The zero value holds none.
type Claims struct {
	txids map[string]bool
	coins map[string]bool
}

// ClaimsOf are the claims a ledger already holds: each line's txid, and the
// coins it spends (Spends); a line whose coins cannot be read claims its
// txid alone.
func ClaimsOf(ps []Payment) *Claims {
	c := &Claims{}
	for _, p := range ps {
		in, err := p.Spends()
		if err != nil {
			in = nil
		}
		c.Take(p.Txid, in)
	}
	return c
}

// Refusal is why a payment of txid spending inputs cannot be taken, or
// Accepted.
func (c *Claims) Refusal(txid string, inputs []string) Claim {
	if c.txids[txid] {
		return Replayed
	}
	if len(inputs) == 0 {
		return Conflict
	}
	for _, i := range inputs {
		if c.coins[i] {
			return Conflict
		}
	}
	return Accepted
}

// Take records a payment as taken.
func (c *Claims) Take(txid string, inputs []string) {
	if c.txids == nil {
		c.txids, c.coins = map[string]bool{}, map[string]bool{}
	}
	c.txids[txid] = true
	for _, i := range inputs {
		c.coins[i] = true
	}
}

// Claim takes a payment unless Refusal refuses it, and answers which.
func (c *Claims) Claim(txid string, inputs []string) Claim {
	if no := c.Refusal(txid, inputs); no != Accepted {
		return no
	}
	c.Take(txid, inputs)
	return Accepted
}

// say writes one line to w, filtered for the terminal.
func say(w io.Writer, s string) {
	if w != nil {
		fmt.Fprintln(w, termsafe.Text(s))
	}
}
