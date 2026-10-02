/**
 * The carrier: one transaction that is never mined, whose record output
 * holds an application's payload as a signed PushDrop [payload] under the
 * producer's derivation, and whose txid is the commitment a mined token can
 * carry. The twin of the Go package carrier's Decode and Validate.
 *
 * A carrier is kept off the chain by BRC-60's device turned the other way
 * round: a far-future nLockTime with a non-final input makes it unmineable
 * in practice, while SPV still verifies it through its funding parent.
 * Nothing in the SDK's verification path reads finality, so the engine admits
 * it like any other object; the topic manager is what refuses one that could
 * reach the chain.
 *
 * The application supplies what is its own: a PayloadCodec, which decides
 * which PushDrop outputs hold its payload and what a valid one is, and
 * lockingKeyFor, which names the key a payload's output must be locked to.
 */
import { PushDrop, type LockingScript, type PublicKey, type Script, type Transaction } from '@bsv/sdk'
import { verifyFieldSignature } from './fieldsig.js'
import { decodeStrictPushDrop } from './pushdrop.js'

/**
 * 2100-01-01T00:00:00Z. With an input whose sequence is below the maximum, a
 * transaction with this nLockTime is not mineable before then. Frozen at the
 * first mint: a reader refuses anything lower.
 */
export const LockTime = 4102444800

/** A final input; any carrier input at this value makes the locktime moot. */
export const MaxSequence = 0xffffffff

/**
 * Why a carrier was refused. The vocabulary is small and fixed because a
 * topic manager counts it as a metric label: an unbounded reason would be a
 * cardinality bomb fed by whoever publishes objects.
 */
export type CarrierRefusal = 'not-pushdrop' | 'bad-record' | 'bad-lock' | 'bad-sig' | 'mineable' | 'non-canonical-unlocking' | 'other'

/**
 * The one sighash type a carrier's input is signed with, SIGHASH_ALL|FORKID,
 * as the Go Mint signs it.
 */
export const SigHashType = 0x41

/** The order n of secp256k1's group. */
const order = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n

/**
 * What a codec makes of the first field of a two-field PushDrop:
 * `not-payload` is somebody else's output and is skipped; `bad-payload`
 * claims to be this application's payload and is not a well-formed one,
 * which refuses the whole carrier.
 */
export type PayloadInspection<P> = { kind: 'payload'; payload: P } | { kind: 'not-payload' } | { kind: 'bad-payload'; detail: string }

export interface PayloadCodec<P> {
  /**
   * Classify and decode the pushed bytes. Throws only on a fault that is not
   * the payload's, which propagates rather than being counted as a refusal.
   */
  inspect(bytes: Uint8Array): PayloadInspection<P>
  /** The payload's own rules. Throws when it breaks them. */
  validate(payload: P): void
}

/**
 * The key a payload's record output must be locked to. Throws when the
 * payload names no key that has a derivation, which refuses as a bad lock.
 */
export type LockingKeyFor<P> = (payload: P) => PublicKey

/** A record output decoded from its locking script alone. */
export interface CarrierOutput<P> {
  payload: P
  /** The exact bytes pushed, which the field signature covers. */
  payloadBytes: Uint8Array
  lockingKey: PublicKey
  signature: number[]
}

export type ScriptInspection<P> = { kind: 'carrier'; out: CarrierOutput<P> } | { kind: 'not-record' } | { kind: 'bad-record'; detail: string }

/**
 * Decode one locking script as a record output. A script that is not a
 * two-field PushDrop, or whose first field the codec does not take, is simply
 * not a record output; one the codec takes and cannot decode is a bad
 * record, which refuses the whole carrier, and so is one the codec takes
 * that is not the one encoding the template writes (decodeStrictPushDrop),
 * as the Go carrier.Decode refuses it.
 */
export function inspectScript<P>(script: Script, codec: PayloadCodec<P>): ScriptInspection<P> {
  let decoded: { lockingPublicKey: PublicKey; fields: number[][] }
  try {
    decoded = PushDrop.decode(script as LockingScript, 'before')
  } catch {
    return { kind: 'not-record' }
  }
  if (decoded.fields.length !== 2) return { kind: 'not-record' }
  const [s, sig] = decoded.fields
  if (s === undefined || sig === undefined) return { kind: 'not-record' }
  const payloadBytes = Uint8Array.from(s)
  const insp = codec.inspect(payloadBytes)
  if (insp.kind === 'not-payload') return { kind: 'not-record' }
  if (insp.kind === 'bad-payload') return { kind: 'bad-record', detail: insp.detail }
  if (decodeStrictPushDrop(script) === undefined) return { kind: 'bad-record', detail: 'the record output is not the canonical PushDrop' }
  return { kind: 'carrier', out: { payload: insp.payload, payloadBytes, lockingKey: decoded.lockingPublicKey, signature: [...sig] } }
}

/** The payload's own rules. */
export function payloadRefusal<P>(out: CarrierOutput<P>, codec: PayloadCodec<P>): CarrierRefusal | undefined {
  try {
    codec.validate(out.payload)
    return undefined
  } catch {
    return 'bad-record'
  }
}

/** The locking key must be the one lockingKeyFor names for the payload. */
export function lockRefusal<P>(out: CarrierOutput<P>, lockingKeyFor: LockingKeyFor<P>): CarrierRefusal | undefined {
  let want: PublicKey
  try {
    want = lockingKeyFor(out.payload)
  } catch {
    // A payload whose key the SDK cannot parse has no derivation, and so no
    // lock it could match.
    return 'bad-lock'
  }
  return want.toString() === out.lockingKey.toString() ? undefined : 'bad-lock'
}

/** The field signature over sha256(payload) under the locking key. */
export function signatureRefusal<P>(out: CarrierOutput<P>): CarrierRefusal | undefined {
  return verifyFieldSignature(out.lockingKey, Array.from(out.payloadBytes), out.signature) ? undefined : 'bad-sig'
}

/**
 * Unmineability: nLockTime at or above the frozen value and every input
 * non-final. One final input would be enough for the locktime to still apply
 * under consensus, but a reader that accepted a mix would be relying on the
 * exact rule rather than the safe shape, so all of them must be non-final,
 * as the Go validator requires.
 */
export function mineableRefusal(tx: Transaction): CarrierRefusal | undefined {
  if (tx.lockTime < LockTime) return 'mineable'
  if (tx.inputs.length === 0) return 'other'
  for (const input of tx.inputs) {
    if ((input.sequence ?? MaxSequence) === MaxSequence) return 'mineable'
  }
  return undefined
}

/** A DER INTEGER's content: present, not negative, not padded. */
function strictInt(b: Uint8Array): boolean {
  if (b.length === 0 || (b[0]! & 0x80) !== 0) return false
  return !(b.length > 1 && b[0] === 0 && (b[1]! & 0x80) === 0)
}

function toBigInt(b: Uint8Array): bigint {
  let x = 0n
  for (const byte of b) x = (x << 8n) | BigInt(byte)
  return x
}

/**
 * A strict DER ECDSA signature under BIP 66's rules, with R in [1, n-1] and
 * S in [1, n/2]: 8 to 72 bytes, and the one encoding of its (r, s). A
 * signature with another encoding verifies as well as the strict one, so a
 * host that took it would take two byte strings for one signature.
 */
export function strictSignature(der: Uint8Array): boolean {
  if (der.length < 8 || der.length > 72) return false
  if (der[0] !== 0x30 || der[1] !== der.length - 2) return false
  const lenR = der[3]!
  if (der[2] !== 0x02 || lenR === 0 || 6 + lenR > der.length) return false
  const lenS = der[5 + lenR]!
  if (der[4 + lenR] !== 0x02 || lenS === 0 || 6 + lenR + lenS !== der.length) return false
  const r = der.subarray(4, 4 + lenR)
  const s = der.subarray(6 + lenR)
  if (!strictInt(r) || !strictInt(s)) return false
  const R = toBigInt(r)
  const S = toBigInt(s)
  return R > 0n && R < order && S > 0n && S <= order >> 1n
}

/**
 * The canonical unlocking script: exactly one input, whose unlocking script
 * is exactly one minimally encoded data push holding a strict DER ECDSA
 * signature (BIP 66) with R in [1, n-1] and S in [1, n/2], followed by the
 * one sighash byte SigHashType. The twin of the Go carrier.CheckUnlocking.
 *
 * The carrier's txid is its commitment, and a host reads a funding output
 * spent by another txid as the record retracted. The script interpreter
 * accepts a high-S signature, a non-minimal push and an extra push or no-op,
 * so without this check anyone who sees a carrier could spend its funding
 * output under a new txid carrying the same record and retract it. The check
 * is structural: whether the signature satisfies the funding output is SPV's.
 * It reads the transaction alone, so a host can run it before anything else.
 */
export function unlockingRefusal(tx: Transaction): CarrierRefusal | undefined {
  if (tx.inputs.length !== 1) return 'non-canonical-unlocking'
  const u = tx.inputs[0]?.unlockingScript
  if (u === undefined) return 'non-canonical-unlocking'
  const s = Uint8Array.from(u.toBinary())
  // A signature and its sighash byte are 9 to 73 bytes, which a minimal
  // encoding always pushes with the one-byte opcode that is its length.
  const op = s[0]
  if (op === undefined || op < 1 || op > 75 || s.length !== 1 + op) return 'non-canonical-unlocking'
  const sig = s.subarray(1)
  if (sig[sig.length - 1] !== SigHashType) return 'non-canonical-unlocking'
  return strictSignature(sig.subarray(0, sig.length - 1)) ? undefined : 'non-canonical-unlocking'
}

export interface Carrier<P> {
  outputIndex: number
  payload: P
  payloadBytes: Uint8Array
  lockingKey: PublicKey
  /** The commitment: the carrier's txid in hash byte order. */
  c: number[]
}

/**
 * The commitment is the carrier's txid in hash byte order: SHA-256d over the
 * transaction bytes, exactly as a token carries it and as an overlay keys
 * the object. It is NOT the display hex reversed.
 */
export function commitment(tx: Transaction): number[] {
  return tx.hash() as number[]
}

/**
 * Decode and validate a carrier transaction, or say why not.
 *
 * Exactly one output may be a record output: with two, the commitment would
 * name two payloads at once. The order of the checks is part of the
 * contract, because a carrier that breaks two rules is refused for the
 * first: the payload's rules, then unmineability, then the canonical
 * unlocking script (unlockingRefusal), then the lock, then the signature.
 * The rules that need the previous state belong to the lookup service, not
 * here.
 */
export function decodeCarrier<P>(tx: Transaction, codec: PayloadCodec<P>, lockingKeyFor: LockingKeyFor<P>): Carrier<P> | CarrierRefusal {
  let found: (CarrierOutput<P> & { outputIndex: number }) | undefined
  for (let i = 0; i < tx.outputs.length; i++) {
    const out = tx.outputs[i]
    if (out === undefined) continue
    const insp = inspectScript(out.lockingScript, codec)
    if (insp.kind === 'not-record') continue
    if (insp.kind === 'bad-record') return 'bad-record'
    if (found !== undefined) return 'bad-record'
    found = { ...insp.out, outputIndex: i }
  }
  if (found === undefined) return 'not-pushdrop'
  const refused = payloadRefusal(found, codec) ?? mineableRefusal(tx) ?? unlockingRefusal(tx) ?? lockRefusal(found, lockingKeyFor) ?? signatureRefusal(found)
  if (refused !== undefined) return refused
  return {
    outputIndex: found.outputIndex,
    payload: found.payload,
    payloadBytes: found.payloadBytes,
    lockingKey: found.lockingKey,
    c: commitment(tx),
  }
}
