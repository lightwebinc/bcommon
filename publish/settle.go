// Package publish is the publisher's dual submission: one signed transaction,
// two encodings, two transports, and never one writer for both.
//
// The settlement leg (TCPIngress, RPCSettler, Arcade) carries a mined
// transaction as EF bytes to a fabric ingress, or as standard hex to a node's
// RPC. The object leg (Facade) carries a BEEF to an overlay host's submit
// door. The shard-proxy
// locks a TCP connection's grammar from its first four bytes (0xBE 0xEF is a
// BRC-149 stream, E3 E1 F3 E8 is framed, anything else is bare transactions),
// so a BEEF written down the EF socket is not a mistake the peer reports, it
// is a poisoned stream. This package makes that a structural rule rather
// than a prose one: the two legs share no connection, no net.Conn and no
// writer, and this package exports nothing that takes both a
// *transaction.Transaction and a BEEF []byte. TestNoExportedAPITakesBoth
// walks the package's exported declarations to hold it, and
// TestLegsNeverShareASocket runs both legs against recording peers.
package publish

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// Settler delivers a signed transaction for mining. It takes the transaction
// itself so that a Settler can never be handed a BEEF: the only bytes a
// Settler ever writes are ones it encoded from a *transaction.Transaction.
type Settler interface {
	Submit(ctx context.Context, tx *transaction.Transaction) error
	Name() string
}

// TCPIngress writes the transaction's EF (BRC-30) bytes to a fabric ingress
// as a bare transaction stream. There is no acknowledgement: the journal and
// a later WaitMined are the evidence.
type TCPIngress struct {
	// Addr is host:port of the ingress (8725 on a shard-proxy).
	Addr string
	// DialTimeout defaults to 5s.
	DialTimeout time.Duration
	// WriteTimeout defaults to 10s and is overridden by a ctx deadline.
	WriteTimeout time.Duration

	// dial is replaced by tests to observe the connection's writes. It is
	// unexported so a caller cannot route the EF leg through anything else.
	dial func(ctx context.Context, addr string) (net.Conn, error)
}

var _ Settler = (*TCPIngress)(nil)

// Name identifies the leg in the journal.
func (t *TCPIngress) Name() string { return "tcp:" + t.Addr }

// Submit dials a fresh connection, writes the EF bytes in one Write and
// closes. One Write because the stream is self-delimiting: the reader parses
// transaction structure with no length prefix, so a short write leaves it
// mid-transaction and everything after is garbage. A fresh connection per
// Submit because a poisoned stream then has nothing after it to poison; the
// cost is one handshake per transition, and transitions are rare.
func (t *TCPIngress) Submit(ctx context.Context, tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("publish: nil transaction")
	}
	// Encode before dialing: a transaction that cannot be EF-encoded (no
	// source outputs) must not open a connection it then writes nothing to.
	ef, err := tx.EF()
	if err != nil {
		return fmt.Errorf("publish: ef: %w", err)
	}
	dial := t.dial
	if dial == nil {
		dial = func(ctx context.Context, addr string) (net.Conn, error) {
			d := net.Dialer{Timeout: t.DialTimeout}
			if d.Timeout == 0 {
				d.Timeout = 5 * time.Second
			}
			return d.DialContext(ctx, "tcp", addr)
		}
	}
	conn, err := dial(ctx, t.Addr)
	if err != nil {
		return fmt.Errorf("publish: dial %s: %w", t.Addr, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(t.WriteTimeout)
	if t.WriteTimeout == 0 {
		deadline = time.Now().Add(10 * time.Second)
	}
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetWriteDeadline(deadline)

	n, err := conn.Write(ef)
	if err != nil {
		return fmt.Errorf("publish: write %d of %d EF bytes to %s: %w", n, len(ef), t.Addr, err)
	}
	if n != len(ef) {
		return fmt.Errorf("publish: short write to %s: %d of %d EF bytes", t.Addr, n, len(ef))
	}
	return nil
}

// RPCSettler sends the standard serialisation through sendrawtransaction
// and returns the node's answer. This leg has an acknowledgement, which the
// TCP ingress does not.
type RPCSettler struct {
	RPC *nodeapi.RPC
}

var _ Settler = (*RPCSettler)(nil)

// Name identifies the leg in the journal.
func (r *RPCSettler) Name() string { return "rpc:" + r.RPC.URL }

// Submit sends tx.Hex() and returns the node's error verbatim. A txid that
// comes back different from the one we computed is an error too: it would
// mean the node parsed different bytes than we signed.
func (r *RPCSettler) Submit(ctx context.Context, tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("publish: nil transaction")
	}
	got, err := r.RPC.SendRawTransaction(ctx, tx.Hex())
	if err != nil {
		return fmt.Errorf("publish: sendrawtransaction: %w", err)
	}
	if want := tx.TxID().String(); got != "" && got != want {
		return fmt.Errorf("publish: sendrawtransaction acknowledged %s, we sent %s", got, want)
	}
	return nil
}
