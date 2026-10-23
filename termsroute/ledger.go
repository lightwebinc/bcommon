package termsroute

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/payee"
)

// LedgerFile is the ledger's name in a host's state directory.
const LedgerFile = payee.LedgerFile

// Received is a payment the route accepted: what a wallet's
// internalizeAction needs to take it, and what the host decided.
type Received struct {
	Txid string
	// Beef is the payment as Atomic BEEF.
	Beef        []byte
	OutputIndex uint32
	Satoshis    uint64
	// DerivationPrefix and DerivationSuffix are the BRC-29 remittance,
	// base64, and SenderIdentityKey the payer, compressed hex.
	DerivationPrefix  string
	DerivationSuffix  string
	SenderIdentityKey string
	Class             string
	// Inputs are every outpoint the payment spends, "<txid>.<index>", the
	// txid in display order.
	Inputs []string
	// Decision is what the host did: "fast" and "mined" are answered,
	// "hold" is not; empty is the payee's own line. Reason is why.
	Decision string
	Reason   string
}

// SpentOutpoints is every outpoint a transaction spends, "<txid>.<index>".
func SpentOutpoints(tx *transaction.Transaction) []string {
	out := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		src := ""
		if in.SourceTXID != nil {
			src = in.SourceTXID.String()
		} else if in.SourceTransaction != nil {
			src = in.SourceTransaction.TxID().String()
		}
		out = append(out, fmt.Sprintf("%s.%d", src, in.SourceTxOutIndex))
	}
	return out
}

// Layout is where a ledger line puts the host's decision: LayoutPayee after
// at (the Go payee package's order), LayoutSpread before it (the order the
// applications' own routes wrote). A line with no decision is the same in
// both, and is byte for byte what payee.Payment.Line writes.
type Layout string

// The layouts.
const (
	LayoutPayee  Layout = "payee"
	LayoutSpread Layout = "spread"
)

type payeeLine struct {
	Txid              string   `json:"txid"`
	Beef              string   `json:"beef"`
	OutputIndex       uint32   `json:"outputIndex"`
	Satoshis          uint64   `json:"satoshis"`
	DerivationPrefix  string   `json:"derivationPrefix"`
	DerivationSuffix  string   `json:"derivationSuffix"`
	SenderIdentityKey string   `json:"senderIdentityKey"`
	Class             string   `json:"class"`
	Inputs            []string `json:"inputs"`
	At                int64    `json:"at"`
	Decision          string   `json:"decision,omitempty"`
	Reason            string   `json:"reason,omitempty"`
}

type spreadLine struct {
	Txid              string   `json:"txid"`
	Beef              string   `json:"beef"`
	OutputIndex       uint32   `json:"outputIndex"`
	Satoshis          uint64   `json:"satoshis"`
	DerivationPrefix  string   `json:"derivationPrefix"`
	DerivationSuffix  string   `json:"derivationSuffix"`
	SenderIdentityKey string   `json:"senderIdentityKey"`
	Class             string   `json:"class"`
	Inputs            []string `json:"inputs"`
	Decision          string   `json:"decision,omitempty"`
	Reason            string   `json:"reason,omitempty"`
	At                int64    `json:"at"`
}

// LedgerLine is p as a ledger line, newline included; at is Unix seconds.
func LedgerLine(p *Received, at int64, layout Layout) ([]byte, error) {
	inputs := p.Inputs
	if inputs == nil {
		inputs = []string{}
	}
	beef := base64.StdEncoding.EncodeToString(p.Beef)
	var v any
	if layout == LayoutSpread {
		v = spreadLine{p.Txid, beef, p.OutputIndex, p.Satoshis, p.DerivationPrefix, p.DerivationSuffix, p.SenderIdentityKey, p.Class, inputs, p.Decision, p.Reason, at}
	} else {
		v = payeeLine{p.Txid, beef, p.OutputIndex, p.Satoshis, p.DerivationPrefix, p.DerivationSuffix, p.SenderIdentityKey, p.Class, inputs, at, p.Decision, p.Reason}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ReadLine is a ledger line as a host reads it at start: the keys the host
// itself reads, unchecked beyond their types. Inputs are the line's own,
// or else those read from its transaction; empty when neither can be read.
type ReadLine struct {
	Txid              string
	Inputs            []string
	Decision          string
	Beef              string
	SenderIdentityKey string
	Satoshis          uint64
	At                int64
}

// ParseLedgerLine reads one line, or (nil, false) for one that is no
// payment (cut short by a crash, whose question was never answered).
func ParseLedgerLine(line []byte) (*ReadLine, bool) {
	var v struct {
		Txid              *string         `json:"txid"`
		Inputs            json.RawMessage `json:"inputs"`
		Beef              *string         `json:"beef"`
		Decision          *string         `json:"decision"`
		SenderIdentityKey *string         `json:"senderIdentityKey"`
		Satoshis          *uint64         `json:"satoshis"`
		At                *int64          `json:"at"`
	}
	if err := json.Unmarshal(line, &v); err != nil || v.Txid == nil {
		return nil, false
	}
	out := &ReadLine{Txid: *v.Txid, Inputs: []string{}}
	var inputs []string
	if v.Inputs != nil && json.Unmarshal(v.Inputs, &inputs) == nil && inputs != nil {
		out.Inputs = inputs
	} else if v.Beef != nil {
		if raw, err := base64.StdEncoding.DecodeString(*v.Beef); err == nil {
			if _, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound); err == nil && tx != nil {
				out.Inputs = SpentOutpoints(tx)
			}
		}
	}
	if v.Decision != nil {
		switch *v.Decision {
		case "fast", "hold", "mined":
			out.Decision = *v.Decision
		}
	}
	if v.Beef != nil {
		out.Beef = *v.Beef
	}
	if v.SenderIdentityKey != nil {
		out.SenderIdentityKey = *v.SenderIdentityKey
	}
	if v.Satoshis != nil {
		out.Satoshis = *v.Satoshis
	}
	if v.At != nil {
		out.At = *v.At
	}
	return out, true
}

// Claim is what a receiver says of a payment offered to it.
type Claim = payee.Claim

// The claims.
const (
	Accepted = payee.Accepted
	Replayed = payee.Replayed
	Conflict = payee.Conflict
)

// taken is the payments a receiver took, by txid and by the coins they
// spend. Two transactions spending one coin can both verify, and at most
// one can ever be settled: the second is a conflict, whatever its txid. A
// payment held for confirmation keeps its coins, and is answered once,
// when it mines.
type taken struct {
	answered map[string]bool
	held     map[string]bool
	coins    map[string]string
}

func (t *taken) init() {
	if t.answered == nil {
		t.answered, t.held, t.coins = map[string]bool{}, map[string]bool{}, map[string]string{}
	}
}

func (t *taken) refusal(txid string, inputs []string) Claim {
	t.init()
	if t.answered[txid] {
		return Replayed
	}
	if len(inputs) == 0 {
		return Conflict
	}
	for _, in := range inputs {
		if by, ok := t.coins[in]; ok && by != txid {
			return Conflict
		}
	}
	return Accepted
}

func (t *taken) take(txid string, inputs []string, decision string) {
	t.init()
	if decision == "hold" {
		t.held[txid] = true
	} else {
		delete(t.held, txid)
		t.answered[txid] = true
	}
	for _, in := range inputs {
		t.coins[in] = txid
	}
}

// Receiver is where accepted payments go. Claim records p and answers
// Accepted, atomically, unless its txid was claimed before (Replayed) or it
// spends a coin an accepted payment spent (Conflict): one payment buys one
// question, and one coin pays once.
type Receiver interface {
	// Refusal is why p cannot be taken, before it is broadcast; Accepted
	// when it can.
	Refusal(txid string, inputs []string) Claim
	Claim(p *Received) Claim
}

// MemoryReceiver keeps payments in memory, for tests.
type MemoryReceiver struct {
	mu       sync.Mutex
	t        taken
	Payments []*Received
}

// Refusal implements Receiver.
func (m *MemoryReceiver) Refusal(txid string, inputs []string) Claim {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t.refusal(txid, inputs)
}

// Claim implements Receiver.
func (m *MemoryReceiver) Claim(p *Received) Claim {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.t.refusal(p.Txid, p.Inputs); c != Accepted {
		return c
	}
	m.t.take(p.Txid, p.Inputs, p.Decision)
	m.Payments = append(m.Payments, p)
	return Accepted
}

// FastLine is a fast payment read from the ledger at start: watched again
// until it mines.
type FastLine struct {
	Beef              []byte
	SenderIdentityKey string
	Satoshis          uint64
	At                time.Time
}

// Ledger is payments as a file, payments.jsonl in a directory, one line
// each, flushed to disk before the question is answered: what the
// operator's wallet internalizes. The txids already in the file are
// claimed, and so are the coins they spend; a line written before lines
// carried their inputs is read from its transaction.
type Ledger struct {
	Path   string
	layout Layout
	now    func() time.Time
	// Fast are the fast lines read at start, newest last.
	Fast []FastLine

	mu sync.Mutex
	t  taken
	f  *os.File
}

// OpenLedger opens or creates dir/payments.jsonl and reads what it holds.
// A last line cut short is ended with a newline.
func OpenLedger(dir string, layout Layout, now func() time.Time) (*Ledger, error) {
	if now == nil {
		now = time.Now
	}
	if layout == "" {
		layout = LayoutPayee
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := &Ledger{Path: filepath.Join(dir, LedgerFile), layout: layout, now: now}
	text, err := os.ReadFile(l.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), payee.MaxLine)
	for sc.Scan() {
		v, ok := ParseLedgerLine(sc.Bytes())
		if !ok {
			continue
		}
		l.t.take(v.Txid, v.Inputs, v.Decision)
		if v.Decision == "fast" && v.Beef != "" && v.SenderIdentityKey != "" {
			if raw, err := base64.StdEncoding.DecodeString(v.Beef); err == nil {
				l.Fast = append(l.Fast, FastLine{Beef: raw, SenderIdentityKey: v.SenderIdentityKey, Satoshis: v.Satoshis, At: time.Unix(v.At, 0)})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", l.Path, err)
	}
	if len(text) > 0 && text[len(text)-1] != '\n' {
		if err := l.write([]byte("\n")); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *Ledger) write(b []byte) error {
	if l.f == nil {
		f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		l.f = f
	}
	if _, err := l.f.Write(b); err != nil {
		return err
	}
	return l.f.Sync()
}

// Refusal implements Receiver.
func (l *Ledger) Refusal(txid string, inputs []string) Claim {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.t.refusal(txid, inputs)
}

// Claim implements Receiver. A held payment is written once; the same
// payment held again is Accepted without a second line.
func (l *Ledger) Claim(p *Received) Claim {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c := l.t.refusal(p.Txid, p.Inputs); c != Accepted {
		return c
	}
	again := l.t.held[p.Txid] && p.Decision == "hold"
	l.t.take(p.Txid, p.Inputs, p.Decision)
	if again {
		return Accepted
	}
	line, err := LedgerLine(p, l.now().Unix(), l.layout)
	if err != nil {
		return Conflict
	}
	if err := l.write(line); err != nil {
		// The question is not answered for a line that is not on disk.
		return Conflict
	}
	return Accepted
}

// Close closes the file.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

var _ io.Closer = (*Ledger)(nil)
