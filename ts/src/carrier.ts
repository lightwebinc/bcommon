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
export type CarrierRefusal = 'not-pushdrop' | 'bad-record' | 'bad-lock' | 'bad-sig' | 'mineable' | 'other'

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
 * record, which refuses the whole carrier.
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
 * first: the payload's rules, then unmineability, then the lock, then the
 * signature. The rules that need the previous state belong to the lookup
 * service, not here.
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
  const refused = payloadRefusal(found, codec) ?? mineableRefusal(tx) ?? lockRefusal(found, lockingKeyFor) ?? signatureRefusal(found)
  if (refused !== undefined) return refused
  return {
    outputIndex: found.outputIndex,
    payload: found.payload,
    payloadBytes: found.payloadBytes,
    lockingKey: found.lockingKey,
    c: commitment(tx),
  }
}
