/**
 * The producer's builders, the TypeScript twins of the Go library's
 * carrier.FundingLock, carrier.Mint and carrier.SweepAt (without a fee
 * input): a funding output's script, a carrier spending one funding output,
 * and the kill switch over a tree that pays for itself. Every key operation
 * goes through a BRC-100 wallet (getPublicKey and createSignature, through
 * the SDK's PushDrop), so a page that asks a user's wallet writes exactly
 * what the Go producer writes with its embedded one. The bytes are held to
 * testdata/vectors/transactions-v1.json, which the Go library and an
 * independent generator also reproduce.
 *
 * The rest of the Go producer (the coin pool and fee payer, minting trees,
 * keeping unproven transactions, proof collection, recovery) is not here: a
 * wallet-funded tree is minted by the wallet's own createAction, which pays,
 * signs and broadcasts it.
 */
import { PushDrop, Transaction, type LockingScript, type WalletInterface, type WalletProtocol } from '@bsv/sdk'
import { LockTime } from './carrier.js'

/** The carrier's input sequence: below the maximum, so LockTime keeps it unmineable (Go carrier.Sequence). */
export const CarrierSequence = 0

/** What locks the record output and the funding outputs (Go pushdrop.Derivation, carrier.Params' frozen part). */
export interface Derivation {
  protocol: WalletProtocol
  keyID: string
}

/** The wallet calls the builders make. */
export type SigningWallet = Pick<WalletInterface, 'getPublicKey' | 'createSignature'>

const asWallet = (w: SigningWallet): WalletInterface => w as unknown as WalletInterface

/** A PushDrop of fields under the derivation, to the wallet's own key, counterparty anyone (Go Derivation.Lock). */
export async function lockFields(w: SigningWallet, originator: string, d: Derivation, fields: Uint8Array[], sign: boolean): Promise<LockingScript> {
  return await new PushDrop(asWallet(w), originator).lock(
    fields.map((f) => Array.from(f)),
    d.protocol,
    d.keyID,
    'anyone',
    true,
    sign,
    'before',
  )
}

/** The unlocker for an output under the derivation: one signature, SIGHASH_ALL|FORKID (Go Derivation.Unlocker). */
export function unlocker(w: SigningWallet, originator: string, d: Derivation): ReturnType<PushDrop['unlock']> {
  return new PushDrop(asWallet(w), originator).unlock(d.protocol, d.keyID, 'anyone', 'all', false)
}

/**
 * A funding-tree output's script: `<key> OP_CHECKSIG <tag> OP_DROP`, no field
 * signature (Go carrier.FundingLock). An empty tag is refused: the SDK
 * would write OP_0 and read it back as 0x00.
 */
export async function fundingLock(w: SigningWallet, originator: string, d: Derivation, tag: Uint8Array): Promise<LockingScript> {
  if (tag.length === 0) throw new Error('writer: empty funding tag')
  return await lockFields(w, originator, d, [tag], false)
}

/**
 * A carrier for payload spending output vout of funding (Go carrier.Mint):
 * nLockTime LockTime, the input's sequence CarrierSequence, and one output,
 * the signed PushDrop of the payload, carrying the whole input value, so
 * the fee is zero. The caller applies its own payload rules first.
 */
export async function mintCarrier(w: SigningWallet, originator: string, d: Derivation, payload: Uint8Array, funding: Transaction, vout: number): Promise<Transaction> {
  if (!Number.isInteger(vout) || vout < 0 || vout >= funding.outputs.length) throw new Error('writer: funding output out of range')
  const lock = await lockFields(w, originator, d, [payload], true)
  const tx = new Transaction(1, [], [], LockTime)
  tx.addInput({ sourceTransaction: funding, sourceOutputIndex: vout, unlockingScriptTemplate: unlocker(w, originator, d), sequence: CarrierSequence })
  tx.addOutput({ satoshis: funding.outputs[vout]!.satoshis!, lockingScript: lock })
  await tx.sign()
  return tx
}

/** A fee policy (Go mint.Fees): a rate of sats over bytes, rounded up, at least floor, refused above max when max is set. */
export interface Fees {
  sats: number
  bytes: number
  floor: number
  max?: number
}

/** The fee for size bytes under f (Go mint.Fees.For). */
export function feeFor(f: Fees, size: number): number {
  if (!Number.isSafeInteger(size) || size < 0 || !Number.isSafeInteger(f.sats) || f.sats < 0 || !Number.isSafeInteger(f.bytes) || f.bytes <= 0) throw new Error('writer: bad fee rate')
  const n = BigInt(size) * BigInt(f.sats)
  const b = BigInt(f.bytes)
  const fee = Math.max(Number((n + b - 1n) / b), f.floor)
  if (f.max !== undefined && f.max > 0 && fee > f.max) throw new Error(`writer: fee ${fee} for ${size} bytes is above the maximum ${f.max}`)
  return fee
}

/**
 * The kill switch over outputs vouts of tree, the tree paying its own fee
 * (Go carrier.SweepAt with no fee input): one input per output, and output
 * 0 a tombstone (a funding output) carrying what is swept less the fee,
 * which the same loop as Go's settles.
 */
export async function sweep(w: SigningWallet, originator: string, d: Derivation, tag: Uint8Array, tree: Transaction, vouts: number[], fees: Fees): Promise<Transaction> {
  if (vouts.length === 0) throw new Error('writer: a sweep needs outputs')
  const tombstone = await fundingLock(w, originator, d, tag)
  const build = async (fee: number): Promise<Transaction> => {
    const tx = new Transaction()
    let swept = 0
    for (const v of vouts) {
      if (!Number.isInteger(v) || v < 0 || v >= tree.outputs.length) throw new Error(`writer: sweep output ${v} out of range`)
      tx.addInput({ sourceTransaction: tree, sourceOutputIndex: v, unlockingScriptTemplate: unlocker(w, originator, d) })
      swept += tree.outputs[v]!.satoshis!
    }
    if (swept <= fee) throw new Error(`writer: sweep inputs ${swept} cannot pay fee ${fee}`)
    tx.addOutput({ satoshis: swept - fee, lockingScript: tombstone })
    await tx.sign()
    return tx
  }
  let fee = fees.floor
  for (let pass = 0; pass < 6; pass++) {
    const tx = await build(fee)
    const size = tx.toBinary().length
    if (feeFor(fees, size) <= fee) return tx
    fee = feeFor(fees, size + 2 * tx.inputs.length)
  }
  throw new Error('writer: sweep fee did not converge')
}
