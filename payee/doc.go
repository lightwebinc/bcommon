// Package payee is the payee side of BRC-105 payments for priced questions:
// the payee key a host is configured with, the ledger in which a host's
// listener records each payment it accepted, and settle, which takes those
// payments into the payee's coin pool.
//
// A host that prices a question is paid to its payee key: each payment is a
// BRC-29 output to a key derived from it, recorded by the host in its ledger
// (LedgerFile in its state directory) before the question is answered, and
// not broadcast by the host. Until it is settled the payer can still spend
// the coins elsewhere: a payment is money only once settled.
//
// # The payee key and the home
//
// The payee is a home of the embedded wallet (package bwallet): its root key
// in IdentityFile is the payee key, and its coin pool is where settled
// payments go. HomeKey reads that key, KeyLine is the line a host's
// environment takes it in, and CreateKeyFile writes that line to a new file
// at mode 0600, never over an existing one.
//
// # The ledger
//
// The ledger is JSON Lines, one accepted payment a line, appended and flushed
// before the question is answered. A line cut short by a crash is passed
// over: its question was never answered. The format is versioned (LedgerV1,
// LedgerV2); every version is read, a line written by a later version is
// reported and left for a reader that knows it, and Payment.Line writes the
// current version byte for byte as a host writes it. Claims is the host's own
// rule over the ledger: a txid is accepted once (a replay is refused) and a
// coin pays once (a payment that spends a coin an accepted payment spent is
// a conflict); a host answers both 409.
//
// # Settle
//
// Settler takes every payment in one or more ledgers that the payee has not
// settled into its pool. Each is checked to pay the key the payee derives
// for its remittance and payer, verified against the headers, broadcast
// through the settlement leg, waited for until it mines and added to the
// pool, and its txid is recorded as settled. Every payment is broadcast
// before any is waited for, so a run takes about one block however many
// there are; InFlight bounds how many are broadcast and not yet mined at
// once. A payment the network refuses for good (its payer spent an input
// elsewhere, which the purse reads from the leg's answer and from the node's
// view of the inputs through chainview) is reported once, recorded, and
// passed over by later runs. A run is idempotent, so it is safe on a timer:
// the sooner a payment is settled, the shorter the window in which the payer
// can take it back.
//
// The record of what is settled is the application's own state, through
// Record; Book is its JSON shape, which an application embeds in its state
// file. The settle step itself is the purse's (package purse), through Payer.
//
// Every piece of text someone else wrote (a class name, a network's reason)
// is held to one line and filtered for the terminal (Field) before it is
// printed.
package payee
