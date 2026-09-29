package verify_test

import (
	"bytes"
	"math/big"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/verify"
)

const what = "sample 7"

// scene is what one row serves: the answer, the commitment asked for, the
// commitment answered, the identity the caller expects and the header
// source.
type scene struct {
	items   []verify.Item
	want    [32]byte
	got     [32]byte
	id      []byte
	tracker chaintracker.ChainTracker
}

// served is the scene for a carrier asked for by its own commitment.
func (f *fixture) served(tx *transaction.Transaction) scene {
	c := carrier.Commitment(tx)
	return scene{items: []verify.Item{f.item(tx, 0)}, want: c, got: c}
}

// run serves one scene through VerifyCarrier, expecting kind 5 of s.id (id1
// when unset) against s.tracker (the fixture's when unset).
func (f *fixture) run(s scene) (*carrier.Carrier, verify.Code, string, []verify.Step) {
	if s.id == nil {
		s.id = f.id1
	}
	if s.tracker == nil {
		s.tracker = f.tracker
	}
	return verify.VerifyCarrier(f.ctx, s.items, s.want, what, spec(5, s.id), s.tracker)
}

// expand fills {what}, {want} and {got} in a reason template.
func expand(tmpl string, s scene) string {
	return strings.NewReplacer("{what}", what, "{want}", display(s.want), "{got}", display(s.got)).Replace(tmpl)
}

// checkRefusal pins a refusal: the code, the reason, no carrier, and one
// step named after the code carrying the reason; or, for Error, no step.
func checkRefusal(t *testing.T, code, reason string, c *carrier.Carrier, gotCode verify.Code, gotReason string, steps []verify.Step) {
	t.Helper()
	if string(gotCode) != code || gotReason != reason {
		t.Errorf("got %s %q\nwant %s %q", gotCode, gotReason, code, reason)
	}
	if c != nil {
		t.Error("a refusal returned a carrier")
	}
	var want []verify.Step
	if code != "ERROR" {
		want = []verify.Step{{Name: code, OK: false, Detail: reason}}
	}
	if len(steps) != len(want) || (len(want) == 1 && steps[0] != want[0]) {
		t.Errorf("steps %+v, want %+v", steps, want)
	}
}

// A carrier that passes returns itself, VERIFIED, and one step named by the
// label carrying its display txid. It is the positive control every
// refusal row below departs from by one fault.
func TestVerifyCarrierPasses(t *testing.T) {
	f := newFixture(t)
	p := payload(f.id1, 5)
	tx := f.mint(f.w1, p, 2)
	s := f.served(tx)
	c, code, reason, steps := f.run(s)
	if code != verify.Verified || reason != "" || c == nil {
		t.Fatalf("got %s %q, carrier %v", code, reason, c != nil)
	}
	if carrier.Commitment(c.Tx) != s.want || c.OutputIndex != 0 || !bytes.Equal(c.Payload, p) {
		t.Fatalf("returned output %d payload %q", c.OutputIndex, c.Payload)
	}
	want := []verify.Step{{Name: what, OK: true, Detail: display(s.want)}}
	if len(steps) != 1 || steps[0] != want[0] {
		t.Fatalf("steps %+v, want %+v", steps, want)
	}
}

// Every refusal VerifyCarrier gives, in the order it checks. A row whose
// carrier breaks two checks pins which runs first. The unmineable nLockTime
// and a non-final sequence are the carrier's; the rows that break them
// change one.
func TestVerifyCarrierRefusals(t *testing.T) {
	f := newFixture(t)
	const unmineable, nonFinal = carrier.LockTime, carrier.Sequence
	valid := func(salt byte, vout uint32) *transaction.Transaction {
		return f.mint(f.w1, append(payload(f.id1, 5), salt), vout)
	}
	handed := func(p []byte, lockTime, seq uint32) scene {
		return f.served(f.hand(f.w1, f.lock(f.w1, p), 3, lockTime, seq))
	}
	// highS is tx with its signature's S replaced by n - S, which anyone can
	// do without the key: the same record under another txid.
	highS := func(tx *transaction.Transaction) scene {
		flipped := script.Script(flipS(t, *tx.Inputs[0].UnlockingScript))
		tx.Inputs[0].UnlockingScript = &flipped
		return f.served(tx)
	}
	rows := []struct {
		name   string
		build  func() scene
		code   string
		reason string
	}{
		{"no carrier answered", func() scene {
			s := f.served(valid(1, 0))
			s.items = nil
			return s
		}, "NO-TOKEN", "the host holds no carrier {want} for {what}"},
		{"two answered", func() scene {
			s := f.served(valid(2, 0))
			s.items = append(s.items, s.items[0])
			return s
		}, "REFUSED-FORK", "{what}: 2 outputs answered for one carrier"},
		{"three answered", func() scene {
			s := f.served(valid(3, 0))
			s.items = append(s.items, s.items[0], s.items[0])
			return s
		}, "REFUSED-FORK", "{what}: 3 outputs answered for one carrier"},
		{"BEEF does not parse", func() scene {
			s := f.served(valid(4, 0))
			s.items = []verify.Item{{Beef: []byte{1, 2, 3}}}
			return s
		}, "REFUSED-DECODE", "{what}: BEEF does not parse: invalid-version"},
		{"BEEF holds no transaction", func() scene {
			s := f.served(valid(5, 0))
			s.items = []verify.Item{{Beef: []byte{0x01, 0x00, 0xbe, 0xef, 0x00, 0x00}}}
			return s
		}, "REFUSED-DECODE", "{what}: BEEF does not parse: <nil>"},
		{"no record output", func() scene {
			return f.served(f.fund1)
		}, "REFUSED-DECODE", "{what}: not a carrier: carrier: not a carrier: no record output"},
		{"the classifier refuses the output", func() scene {
			return handed([]byte("smp"), unmineable, nonFinal)
		}, "REFUSED-DECODE", "{what}: not a carrier: carrier: record output has the wrong shape: output 0: sample: too short"},
		{"two record outputs", func() scene {
			lock := f.lock(f.w1, payload(f.id1, 5))
			tx := f.hand(f.w1, lock, 3, unmineable, nonFinal)
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: lock})
			return f.served(tx)
		}, "REFUSED-DECODE", "{what}: not a carrier: carrier: not a carrier: more than one record output"},
		{"answered output is not the record output", func() scene {
			tx := valid(6, 0)
			s := f.served(tx)
			s.items = []verify.Item{f.item(tx, 1)}
			return s
		}, "REFUSED-DECODE", "{what}: the answered output 1 is not the record output 0"},
		{"a different carrier than the one asked for", func() scene {
			asked := valid(7, 0)
			s := f.served(valid(8, 1))
			s.want = carrier.Commitment(asked)
			return s
		}, "REFUSED-COMMIT", "{what}: the host answered carrier {got}, not {want}"},
		{"a different carrier at the wrong output: the index first", func() scene {
			asked, other := valid(9, 0), valid(10, 1)
			s := f.served(other)
			s.items = []verify.Item{f.item(other, 1)}
			s.want = carrier.Commitment(asked)
			return s
		}, "REFUSED-DECODE", "{what}: the answered output 1 is not the record output 0"},
		// A carrier that is not the one asked for is refused as that,
		// whatever else is wrong with it: nothing it says about itself is
		// read first.
		{"no identity and not the carrier asked for: the commitment first", func() scene {
			s := handed([]byte("smp\x01short"), unmineable, nonFinal)
			s.want = carrier.Commitment(valid(14, 0))
			return s
		}, "REFUSED-COMMIT", "{what}: the host answered carrier {got}, not {want}"},
		{"an invalid payload and not the carrier asked for: the commitment first", func() scene {
			p := payload(f.id1, 5)
			p[3] = 2
			s := handed(p, unmineable, nonFinal)
			s.want = carrier.Commitment(valid(15, 0))
			return s
		}, "REFUSED-COMMIT", "{what}: the host answered carrier {got}, not {want}"},
		{"mineable and not the carrier asked for: the commitment first", func() scene {
			s := handed(payload(f.id1, 5), 0, nonFinal)
			s.want = carrier.Commitment(valid(16, 0))
			return s
		}, "REFUSED-COMMIT", "{what}: the host answered carrier {got}, not {want}"},
		{"not the kind asked for and not the carrier asked for: the commitment first", func() scene {
			s := f.served(f.mint(f.w1, payload(f.id1, 4), 1))
			s.want = carrier.Commitment(valid(17, 0))
			return s
		}, "REFUSED-COMMIT", "{what}: the host answered carrier {got}, not {want}"},
		{"the payload names no identity", func() scene {
			return handed([]byte("smp\x01short"), unmineable, nonFinal)
		}, "REFUSED-DECODE", "{what}: sample: names no identity"},
		{"no identity and an invalid payload: the identity read first", func() scene {
			return handed([]byte("smp\x02short"), unmineable, nonFinal)
		}, "REFUSED-DECODE", "{what}: sample: names no identity"},
		{"Validate: the payload's own rules", func() scene {
			p := payload(f.id1, 5)
			p[3] = 2
			return handed(p, unmineable, nonFinal)
		}, "REFUSED-DECODE", "{what}: sample: version is not 1"},
		{"Validate: the payload's rules before finality", func() scene {
			p := payload(f.id1, 5)
			p[3] = 2
			return handed(p, 0, nonFinal)
		}, "REFUSED-DECODE", "{what}: sample: version is not 1"},
		{"Validate: mineable by nLockTime", func() scene {
			return handed(payload(f.id1, 5), unmineable-1, nonFinal)
		}, "REFUSED-MINEABLE", "{what}: carrier: mineable; the record could reach the chain: nLockTime 4102444799"},
		{"Validate: mineable by a final input", func() scene {
			return handed(payload(f.id1, 5), unmineable, 0xffffffff)
		}, "REFUSED-MINEABLE", "{what}: carrier: mineable; the record could reach the chain: input 0 is final"},
		{"Validate: no inputs", func() scene {
			tx := transaction.NewTransaction()
			tx.LockTime = unmineable
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: f.lock(f.w1, payload(f.id1, 5))})
			return f.served(tx)
		}, "REFUSED-DECODE", "{what}: carrier: not a carrier: no inputs"},
		{"Validate: a high-S unlocking script", func() scene {
			return highS(valid(18, 0))
		}, "REFUSED-UNLOCKING", "{what}: carrier: unlocking script is not one canonical signature push: S is high"},
		{"Validate: a non-minimal push", func() scene {
			tx := valid(19, 0)
			u := *tx.Inputs[0].UnlockingScript
			wide := script.Script(append([]byte{script.OpPUSHDATA1}, u...))
			tx.Inputs[0].UnlockingScript = &wide
			return f.served(tx)
		}, "REFUSED-UNLOCKING", "{what}: carrier: unlocking script is not one canonical signature push: not a minimal push"},
		{"Validate: mineable before the unlocking script", func() scene {
			return highS(f.hand(f.w1, f.lock(f.w1, payload(f.id1, 5)), 3, 0, nonFinal))
		}, "REFUSED-MINEABLE", "{what}: carrier: mineable; the record could reach the chain: nLockTime 0"},
		{"Validate: the unlocking script before the identity key", func() scene {
			id := bytes.Clone(f.id1)
			id[0] = 0x04
			return highS(f.hand(f.w1, f.lock(f.w1, payload(id, 5)), 3, unmineable, nonFinal))
		}, "REFUSED-UNLOCKING", "{what}: carrier: unlocking script is not one canonical signature push: S is high"},
		{"the unlocking script before the proof", func() scene {
			s := highS(valid(20, 0))
			s.tracker = &goldentest.Tracker{Roots: map[uint32]string{}}
			return s
		}, "REFUSED-UNLOCKING", "{what}: carrier: unlocking script is not one canonical signature push: S is high"},
		{"Validate: identity key does not parse", func() scene {
			id := bytes.Clone(f.id1)
			id[0] = 0x04
			return handed(payload(id, 5), unmineable, nonFinal)
		}, "REFUSED-DECODE", "{what}: carrier: identity key does not parse: invalid magic in compressed pubkey string: 4"},
		{"Validate: mineable before the identity key", func() scene {
			id := bytes.Clone(f.id1)
			id[0] = 0x04
			return handed(payload(id, 5), 0, nonFinal)
		}, "REFUSED-MINEABLE", "{what}: carrier: mineable; the record could reach the chain: nLockTime 0"},
		{"Validate: lock is not the identity's key", func() scene {
			// id1's payload, locked and funded by w2.
			return f.served(f.mint(f.w2, payload(f.id1, 5), 0))
		}, "REFUSED-KEY-DERIVE", "{what}: carrier: locking key is not the identity's record key"},
		{"Validate: field signature", func() scene {
			lock := f.lock(f.w1, payload(f.id1, 5))
			sig := sdkpushdrop.Decode(lock).Fields[1]
			b := bytes.Clone(*lock)
			at := bytes.Index(b, sig)
			if at < 0 {
				t.Fatal("signature not found in the lock")
			}
			b[at+len(sig)-1] ^= 0x01
			bad := script.Script(b)
			return f.served(f.hand(f.w1, &bad, 3, unmineable, nonFinal))
		}, "REFUSED-SIG", "{what}: carrier: field signature does not verify"},
		{"Expect: its code and its reason as given", func() scene {
			return f.served(f.mint(f.w1, payload(f.id1, 4), 0))
		}, "REFUSED-DECODE", "not the kind asked for"},
		{"Expect: a second rule", func() scene {
			s := f.served(f.mint(f.w1, payload(f.id1, 5), 1))
			s.id = f.id2
			return s
		}, "REFUSED-KEY", "names another identity"},
		{"Expect after Validate", func() scene {
			return handed(payload(f.id1, 4), 0, nonFinal)
		}, "REFUSED-MINEABLE", "{what}: carrier: mineable; the record could reach the chain: nLockTime 0"},
		{"Expect before the proof", func() scene {
			s := f.served(f.mint(f.w1, payload(f.id1, 4), 2))
			s.tracker = &goldentest.Tracker{Roots: map[uint32]string{}}
			return s
		}, "REFUSED-DECODE", "not the kind asked for"},
		{"funding parent not proven", func() scene {
			s := f.served(valid(11, 0))
			s.tracker = &goldentest.Tracker{Roots: map[uint32]string{}}
			return s
		}, "REFUSED-BUMP", "{what}: the funding parent is not proven in the header source"},
		{"funding parent missing from the answer", func() scene {
			tx := valid(12, 0)
			c := carrier.Commitment(tx)
			return scene{items: []verify.Item{f.bare(tx)}, want: c, got: c}
		}, "REFUSED-BUMP", "{what}: the funding parent is not proven in the header source"},
		{"input does not satisfy its funding output", func() scene {
			// id2's own carrier, valid in itself, spending id1's tree.
			tx := transaction.NewTransaction()
			tx.LockTime = unmineable
			tx.AddInputFromTx(f.fund1, 3, sample.Unlocker(f.ctx, f.w2, originator))
			tx.Inputs[0].SequenceNumber = nonFinal
			tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: f.lock(f.w2, payload(f.id2, 5))})
			if err := tx.Sign(); err != nil {
				t.Fatal(err)
			}
			s := f.served(tx)
			s.id = f.id2
			return s
		}, "REFUSED-SIG", "{what}: the input does not satisfy its funding output"},
		{"header source down", func() scene {
			s := f.served(valid(13, 0))
			s.tracker = &downTracker{}
			return s
		}, "ERROR", "{what}: could not verify: header source unavailable"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			s := r.build()
			c, code, reason, steps := f.run(s)
			checkRefusal(t, r.code, expand(r.reason, s), c, code, reason, steps)
		})
	}
}

// A carrier refused on its content costs no header lookup; the same header
// source IS asked once the carrier passes, and a missing one is refused
// rather than replaced by the SDK's public default.
func TestVerifyCarrierHeaderSource(t *testing.T) {
	f := newFixture(t)
	wrongKind := f.served(f.mint(f.w1, payload(f.id1, 4), 0))
	good := f.served(f.mint(f.w1, payload(f.id1, 5), 1))

	down := &downTracker{}
	wrongKind.tracker = down
	c, code, reason, steps := f.run(wrongKind)
	checkRefusal(t, "REFUSED-DECODE", "not the kind asked for", c, code, reason, steps)
	if down.calls != 0 {
		t.Errorf("header source asked %d time(s) for a carrier refused on its content", down.calls)
	}
	good.tracker = down
	c, code, reason, steps = f.run(good)
	checkRefusal(t, "ERROR", what+": could not verify: header source unavailable", c, code, reason, steps)
	if down.calls == 0 {
		t.Error("the header source was not asked for a carrier that passed its own checks")
	}

	// A nil tracker: content refusals are still made, and a carrier that
	// passes them gets no verdict.
	c, code, reason, steps = verify.VerifyCarrier(f.ctx, wrongKind.items, wrongKind.want, what, spec(5, f.id1), nil)
	checkRefusal(t, "REFUSED-DECODE", "not the kind asked for", c, code, reason, steps)
	c, code, reason, steps = verify.VerifyCarrier(f.ctx, good.items, good.want, what, spec(5, f.id1), nil)
	checkRefusal(t, "ERROR", what+": could not verify: verify: no chain tracker; refusing to verify against a default", c, code, reason, steps)
}

// A spec missing any hook is the caller's mistake and no verdict on the
// carrier, so it is ERROR before anything is read, the item count included:
// with no answer or two it is still ERROR, never the NO-TOKEN or
// REFUSED-FORK a complete spec would give there. The full spec over the one
// answer passes.
func TestVerifyCarrierIncompleteSpec(t *testing.T) {
	f := newFixture(t)
	s := f.served(f.mint(f.w1, payload(f.id1, 5), 0))
	answers := map[string][]verify.Item{
		"one answer":  s.items,
		"no answer":   nil,
		"two answers": {s.items[0], s.items[0]},
	}
	for name, broken := range map[string]func(*verify.CarrierSpec){
		"no classifier":        func(sp *verify.CarrierSpec) { sp.Classify = nil },
		"no identity reader":   func(sp *verify.CarrierSpec) { sp.IdentityOf = nil },
		"no expectations":      func(sp *verify.CarrierSpec) { sp.Expect = nil },
		"no payload validator": func(sp *verify.CarrierSpec) { sp.Params.ValidatePayload = nil },
	} {
		for answer, items := range answers {
			t.Run(name+", "+answer, func(t *testing.T) {
				sp := spec(5, f.id1)
				broken(&sp)
				c, code, reason, steps := verify.VerifyCarrier(f.ctx, items, s.want, what, sp, f.tracker)
				checkRefusal(t, "ERROR", "verify: incomplete carrier spec", c, code, reason, steps)
			})
		}
	}
	c, code, _, _ := verify.VerifyCarrier(f.ctx, s.items, s.want, what, spec(5, f.id1), f.tracker)
	if code != verify.Verified || c == nil {
		t.Fatalf("control: %s", code)
	}
}

// flipS rewrites a canonical unlocking script's signature to the other S
// that verifies, n - S, strictly encoded.
func flipS(t *testing.T, unlocking []byte) []byte {
	t.Helper()
	if len(unlocking) < 2 || int(unlocking[0]) != len(unlocking)-1 {
		t.Fatalf("not one direct push: %x", unlocking)
	}
	sig := unlocking[1:]
	der, hashType := sig[:len(sig)-1], sig[len(sig)-1]
	lenR := int(der[3])
	r, s := der[4:4+lenR], new(big.Int).SetBytes(der[6+lenR:])
	flipped := new(big.Int).Sub(ec.S256().N, s).Bytes()
	if flipped[0]&0x80 != 0 {
		flipped = append([]byte{0}, flipped...)
	}
	body := append([]byte{0x02, byte(len(r))}, r...)
	body = append(append(body, 0x02, byte(len(flipped))), flipped...)
	out := append([]byte{0x30, byte(len(body))}, body...)
	out = append(out, hashType)
	return append([]byte{byte(len(out))}, out...)
}
