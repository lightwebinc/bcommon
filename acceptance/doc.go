// Package acceptance decides how much evidence a payment needs before the
// receiver acts on it, and gathers that evidence.
//
// A payment is worth what it pays only once it is mined: until then its
// payer can spend the same coins elsewhere. Waiting for a block on every
// payment makes every paid answer as slow as a block, while acting on a
// payment at once makes the receiver carry every double spend. The rule
// this package applies is a value discriminator:
//
//   - a payment at or below a threshold takes the fast path: its ancestry
//     verifies to mined proofs against the receiver's headers, it is well
//     formed, final and pays what it must, the receiver broadcasts it
//     itself, the network's broadcaster answers it accepted with no
//     double-spend status, and the node's spend view names no other
//     spender, through an optional conflict-watch window;
//   - a payment above the threshold is held for confirmation: it is
//     broadcast the same way, and acted on only once it is mined with a
//     merkle proof that verifies against the receiver's headers;
//   - a payment that fails a check is refused.
//
// The threshold is in satoshis. It is a static value, or one converted from
// a fiat amount through a PriceSource the application supplies; a price
// that is unknown, zero or stale makes the threshold zero, so every payment
// is held: the package fails toward waiting, never toward trusting.
//
// Many small payments add up, so the fast path is also bounded per payer
// and in total: an Exposure counts the satoshis taken on the fast path and
// not yet mined within a rolling window, and a payment that would carry
// either sum past its limit is held instead. The receiver decides, always:
// a payer may ask to be held, never to be fast.
//
// A payment taken on the fast path is watched until it mines (Monitor). One
// that the network refuses, or whose input the node shows spent by another
// transaction, is reported to the application's hook and its payer flagged,
// so that the payer's later payments are held for confirmation. What else
// follows (revoking a service, telling an operator) is the application's.
//
// The zero Policy holds every payment: a receiver that configures nothing
// waits for a block, as it did before this package.
package acceptance
