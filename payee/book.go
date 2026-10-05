package payee

import "slices"

// Record is where a payee keeps what settle did, across runs: the payments
// it settled and those the network refused for good. Settle asks before it
// touches a payment and records each outcome as it comes, so a run cut short
// loses nothing it finished. Each Record method that records persists
// before it returns.
type Record interface {
	IsSettled(txid string) bool
	IsUnsettleable(txid string) bool
	RecordSettled(txid string) error
	RecordUnsettleable(txid, why string) error
}

// Book is a payee's record in its state file's JSON: an application embeds
// it in its state struct, where the two keys keep their place and their
// bytes.
type Book struct {
	// Settled are the txids of payments to this identity, as a host's
	// payee, that settle internalized.
	Settled []string `json:"settled,omitempty"`
	// Unsettleable are payments to this identity, as a payee, that the
	// network refused for good (the payer spent the inputs elsewhere):
	// settle reports each once and then passes it over.
	Unsettleable []Unsettleable `json:"unsettleable,omitempty"`
}

// Unsettleable is a payment the network refused, and why.
type Unsettleable struct {
	Txid string `json:"txid"`
	Why  string `json:"why"`
}

// IsSettled reports whether txid is a payment recorded as settled.
func (b *Book) IsSettled(txid string) bool {
	return slices.Contains(b.Settled, txid)
}

// IsUnsettleable reports whether txid is a payment recorded as refused.
func (b *Book) IsUnsettleable(txid string) bool {
	return slices.ContainsFunc(b.Unsettleable, func(u Unsettleable) bool { return u.Txid == txid })
}

// AddSettled records txid as settled, once.
func (b *Book) AddSettled(txid string) {
	if !b.IsSettled(txid) {
		b.Settled = append(b.Settled, txid)
	}
}

// AddUnsettleable records txid as refused, once.
func (b *Book) AddUnsettleable(txid, why string) {
	if !b.IsUnsettleable(txid) {
		b.Unsettleable = append(b.Unsettleable, Unsettleable{Txid: txid, Why: why})
	}
}

// Saved is b as a Record that calls save after each change: the state file
// b is embedded in, written whole.
func Saved(b *Book, save func() error) Record {
	return &saved{b, save}
}

type saved struct {
	*Book
	save func() error
}

func (s *saved) RecordSettled(txid string) error {
	s.AddSettled(txid)
	return s.save()
}

func (s *saved) RecordUnsettleable(txid, why string) error {
	s.AddUnsettleable(txid, why)
	return s.save()
}
