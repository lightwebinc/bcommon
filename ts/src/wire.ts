/**
 * The BEEF a host admits a mined chain token in, read exactly as declared on
 * the wire, and the token's own output: the twin of the Go package
 * chaintoken.
 *
 * A chain token is a mined PushDrop output that the next token spends. A
 * host that admits one reads its predecessor from the parent the
 * submission's BEEF carries, never from what the topic holds, so a verdict
 * depends on the submission's bytes and the host's headers alone. That only
 * works when a BEEF holds exactly what the object needs: nobody can attach
 * anything to it, and two hosts given the same bytes decide the same.
 *
 * Before anything is parsed the bytes are walked as the Go guard walks
 * them (checkBEEF): every count and length a BEEF declares is held to the
 * bytes present, so nothing it declares can make a parser allocate or loop
 * for more than it holds.
 */
import { MerklePath, PublicKey, Transaction, Utils, type ChainTracker } from '@bsv/sdk'
import { strictSignature } from './carrier.js'
import { bytesEqual } from './cbor.js'
import { verifyFieldSignature } from './fieldsig.js'
import { pushDropScript, pushFields } from './script.js'

/** A BEEF bound for a host that names none, and the floor for a host's own: 256 KiB. */
export const DefaultMaxBEEF = 256 << 10

const V1 = 0xefbe0001
const V2 = 0xefbe0002
const ATOMIC = 0x01010101

// The smallest encodings, which make every count bound a floor: see the Go
// guard.
const minInputBytes = 32 + 4 + 1 + 4
const minOutputBytes = 8 + 1
const minTxBytes = 4 + 1 + minInputBytes + 1 + 4
const minBEEFBump = 3
const minV1Entry = minTxBytes + 1
const minV2Entry = 1 + 32
const minLeafBytes = 2
const maxTreeHeight = 64

/**
 * A BEEF that is over its bound, does not parse as exactly one BEEF with
 * nothing after it, lacks its subject, or holds anything but what the
 * object needs. The twin of the Go chaintoken.ErrBEEF.
 */
export class BeefRefusal extends Error {
  constructor(detail: string) {
    super(detail)
    this.name = 'BeefRefusal'
  }
}

const hexOf = (b: Uint8Array | number[]): string => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')

/** A cursor over the bytes that only ever moves within them. */
class Cursor {
  pos = 0
  constructor(readonly b: Uint8Array) {}

  get remaining(): number {
    return this.b.length - this.pos
  }

  skip(n: number): void {
    if (n < 0 || this.remaining < n) throw new BeefRefusal('ends mid-structure')
    this.pos += n
  }

  byte(): number {
    if (this.remaining < 1) throw new BeefRefusal('ends mid-structure')
    return this.b[this.pos++]!
  }

  uint32(): number {
    if (this.remaining < 4) throw new BeefRefusal('ends mid-structure')
    const b = this.b
    const p = this.pos
    this.pos += 4
    return (b[p]! | (b[p + 1]! << 8) | (b[p + 2]! << 16)) + b[p + 3]! * 0x1000000
  }

  /**
   * A Bitcoin VarInt. The eight-byte form is returned as a number that is
   * exact up to 2^53 and only ever compared with what remains, which is far
   * below that, so a larger value is refused wherever it is a count.
   */
  varInt(): number {
    const p = this.byte()
    if (p === 0xfd) {
      if (this.remaining < 2) throw new BeefRefusal('ends mid-structure')
      const v = this.b[this.pos]! | (this.b[this.pos + 1]! << 8)
      this.pos += 2
      return v
    }
    if (p === 0xfe) return this.uint32()
    if (p === 0xff) {
      const lo = this.uint32()
      const hi = this.uint32()
      return hi * 0x100000000 + lo
    }
    return p
  }

  /** Whether n items of at least each bytes could be encoded in what remains. */
  fits(n: number, each: number): boolean {
    return n <= Math.floor(this.remaining / each)
  }

  /** One BUMP; its tree height. */
  bump(): number {
    this.varInt() // block height
    const treeHeight = this.byte()
    if (treeHeight > maxTreeHeight) throw new BeefRefusal(`a BUMP declares tree height ${treeHeight}`)
    for (let lv = 0; lv < treeHeight; lv++) {
      const leaves = this.varInt()
      if (!this.fits(leaves, minLeafBytes)) throw new BeefRefusal(`a BUMP level declares ${leaves} leaves, ${this.remaining} bytes remain`)
      for (let i = 0; i < leaves; i++) {
        this.varInt() // offset
        if ((this.byte() & 1) === 0) this.skip(32) // not a duplicate: a hash follows
      }
    }
    return treeHeight
  }

  /** One raw transaction. */
  tx(): void {
    if (this.remaining < minTxBytes) throw new BeefRefusal('ends mid-structure')
    this.pos += 4 // version
    const nIn = this.varInt()
    if (nIn === 0) throw new BeefRefusal('a transaction of no inputs')
    if (!this.fits(nIn, minInputBytes)) throw new BeefRefusal(`declares ${nIn} inputs, ${this.remaining} bytes remain`)
    for (let i = 0; i < nIn; i++) {
      this.skip(32 + 4) // outpoint
      const l = this.varInt()
      if (l > this.remaining) throw new BeefRefusal(`an input script declares ${l} bytes, ${this.remaining} remain`)
      this.pos += l
      this.skip(4) // sequence
    }
    const nOut = this.varInt()
    if (!this.fits(nOut, minOutputBytes)) throw new BeefRefusal(`declares ${nOut} outputs, ${this.remaining} bytes remain`)
    for (let i = 0; i < nOut; i++) {
      this.skip(8) // value
      const l = this.varInt()
      if (l > this.remaining) throw new BeefRefusal(`an output script declares ${l} bytes, ${this.remaining} remain`)
      this.pos += l
    }
    this.skip(4) // locktime
  }

  /** A V1 or V2 BEEF, not another Atomic wrapper. */
  beef(): void {
    const v = this.uint32()
    if (v !== V1 && v !== V2) throw new BeefRefusal('the version is not BEEF V1 or V2')
    const nBumps = this.varInt()
    if (!this.fits(nBumps, minBEEFBump)) throw new BeefRefusal(`declares ${nBumps} BUMPs, ${this.remaining} bytes remain`)
    for (let i = 0; i < nBumps; i++) {
      if (this.bump() === 0) throw new BeefRefusal(`BUMP ${i} has no levels`)
    }
    const nTx = this.varInt()
    if (!this.fits(nTx, v === V1 ? minV1Entry : minV2Entry)) throw new BeefRefusal(`declares ${nTx} transactions, ${this.remaining} bytes remain`)
    if (nTx === 0) throw new BeefRefusal('no transactions')
    const bumpIndex = (i: number): void => {
      const idx = this.varInt()
      if (idx >= nBumps) throw new BeefRefusal(`transaction ${i} names BUMP ${idx} of ${nBumps}`)
    }
    for (let i = 0; i < nTx; i++) {
      if (v === V1) {
        this.tx()
        const has = this.byte()
        if (has === 1) bumpIndex(i)
        else if (has !== 0) throw new BeefRefusal(`transaction ${i} has-BUMP byte ${has}`)
        continue
      }
      const format = this.byte()
      if (format === 2) {
        this.skip(32)
        continue
      }
      if (format === 1) bumpIndex(i)
      else if (format !== 0) throw new BeefRefusal(`transaction ${i} data format ${format}`)
      this.tx()
    }
  }
}

/**
 * Walks a BEEF of at most bound bytes and returns why it is refused, or
 * undefined: anything whose declared counts or lengths overrun the bytes
 * present, or that leaves bytes over. It admits BEEF V1 (BRC-62), BEEF V2
 * (BRC-96) with its txid-only entries, and Atomic BEEF (BRC-95) around
 * either, and allocates nothing. The twin of the Go guard.CheckBEEF: a
 * structural check, not a parser. It refuses trailing bytes, a BEEF of no
 * transactions and a transaction of no inputs.
 */
export function checkBEEF(raw: Uint8Array, bound: number): string | undefined {
  if (raw.length > bound) return `${raw.length} bytes, max ${bound}`
  const c = new Cursor(raw)
  try {
    if (raw.length >= 4 && (raw[0]! | (raw[1]! << 8) | (raw[2]! << 16)) + raw[3]! * 0x1000000 === ATOMIC) c.skip(4 + 32)
    c.beef()
  } catch (e) {
    if (e instanceof BeefRefusal) return e.message
    throw e
  }
  return c.remaining === 0 ? undefined : `${c.remaining} trailing bytes`
}

/** One transaction as the BEEF declares it. */
export interface WireEntry {
  /**
   * The transaction, undefined for a txid-only entry. Its merklePath is the
   * BUMP the entry names, and each input's sourceTransaction is the earlier
   * entry that holds it, when one does.
   */
  tx?: Transaction
  /** Display order. */
  txid: string
  /** The index of its BUMP, or -1. */
  bump: number
  /** A BRC-96 txid-only entry. */
  txidOnly: boolean
}

/** A BEEF exactly as declared on the wire: its form, its BUMPs and its transactions in order. */
export interface Wire {
  /** An Atomic BEEF (BRC-95). */
  atomic: boolean
  /** The Atomic BEEF's subject, else the last entry's txid; display order. */
  subject: string
  bumps: MerklePath[]
  entries: WireEntry[]
  /** Each txid's entry; the later one when a BEEF lists a txid twice. */
  byTxid: Map<string, WireEntry>
}

/** The parse proper, after the walk: the SDK's readers over bytes whose every count fits. */
function parseInto(w: Wire, raw: Uint8Array): void {
  const r = new Utils.Reader(Array.from(raw))
  let v = r.readUInt32LE()
  if (v === ATOMIC) {
    w.atomic = true
    w.subject = hexOf(Uint8Array.from(r.read(32)).reverse())
    v = r.readUInt32LE()
  }
  const nb = r.readVarIntNum()
  for (let i = 0; i < nb; i++) w.bumps.push(MerklePath.fromReader(r, false, false))
  const nt = r.readVarIntNum()
  for (let i = 0; i < nt; i++) {
    const e: WireEntry = { txid: '', bump: -1, txidOnly: false }
    const format = v === V2 ? r.readUInt8() : 0
    if (format === 2) {
      e.txidOnly = true
      e.txid = hexOf(Uint8Array.from(r.read(32)).reverse())
    } else {
      if (format === 1) e.bump = r.readVarIntNum()
      const tx = Transaction.fromReader(r)
      if (v === V1 && r.readUInt8() === 1) e.bump = r.readVarIntNum()
      if (e.bump >= 0) tx.merklePath = w.bumps[e.bump]
      e.tx = tx
      e.txid = tx.id('hex')
    }
    w.entries.push(e)
  }
  if (r.pos !== raw.length) throw new BeefRefusal('the parser and the walk disagree about where the BEEF ends')
}

/**
 * Reads a BEEF V1, V2 or Atomic BEEF of at most bound bytes as declared on
 * the wire, before any parser merges two proofs of one block or collapses a
 * transaction listed twice. The walk of checkBEEF runs first. Each
 * transaction's inputs are linked to the earlier entries holding their
 * sources, and each proven transaction to its BUMP. Paths are read as
 * written, without the SDK's offset and root checks: the shapes below hold
 * them to what the object needs, and the header check to the truth.
 *
 * Every refusal is a BeefRefusal: over the bound, not one well-formed BEEF,
 * bytes after it, no transaction, or no subject transaction carried whole.
 */
export function readWire(raw: Uint8Array, bound: number = DefaultMaxBEEF): Wire {
  const why = checkBEEF(raw, bound)
  if (why !== undefined) throw new BeefRefusal(why)
  const w: Wire = { atomic: false, subject: '', bumps: [], entries: [], byTxid: new Map() }
  try {
    parseInto(w, raw)
  } catch (e) {
    if (e instanceof BeefRefusal) throw e
    // What the SDK's own readers refuse in bytes the walk passed: a path
    // listing one offset twice, or a number past 53 bits.
    throw new BeefRefusal(e instanceof Error ? e.message : String(e))
  }
  if (!w.atomic) w.subject = w.entries.at(-1)!.txid
  for (const e of w.entries) {
    if (e.tx !== undefined) {
      for (const input of e.tx.inputs) {
        const src = w.byTxid.get(input.sourceTXID ?? '')
        if (src?.tx !== undefined) input.sourceTransaction = src.tx
      }
    }
    w.byTxid.set(e.txid, e)
  }
  if (w.byTxid.get(w.subject)?.tx === undefined) throw new BeefRefusal('no subject transaction')
  return w
}

/** The subject transaction, which readWire has held to be present and carried whole. */
export const subjectTx = (w: Wire): Transaction => w.byTxid.get(w.subject)!.tx!

/** Whether the BEEF has a BRC-96 txid-only entry. Every shape here carries each transaction whole. */
export const anyTxidOnly = (w: Wire): boolean => w.entries.some((e) => e.txidOnly)

/** txid at the lowest level, flagged as a txid. */
function txidOffset(mp: MerklePath, txid: string): number | undefined {
  return mp.path[0]?.find((e) => e.hash === txid && e.txid === true)?.offset
}

/**
 * Whether mp is the union of the minimal paths of a and b (a === b for one
 * transaction): at the lowest level a and b flagged as txids and nothing
 * else flagged, and their siblings; above that only the siblings of their
 * ancestors. A sibling that is itself an ancestor of the other may be left
 * out, as BEEF writers do when they merge two paths of one block; every
 * other sibling must be there; nothing else may be.
 */
export function mergedPath(mp: MerklePath, a: string, b: string): boolean {
  if (mp.path.length === 0) return false
  const sets: Array<Map<number, boolean>> = []
  for (const level of mp.path) {
    const m = new Map<number, boolean>()
    for (const e of level) {
      if (m.has(e.offset)) return false
      m.set(e.offset, e.txid === true)
    }
    sets.push(m)
  }
  const oa = txidOffset(mp, a)
  const ob = txidOffset(mp, b)
  if (oa === undefined || ob === undefined) return false
  let flagged = 0
  for (const t of sets[0]!.values()) if (t) flagged++
  if (flagged !== (a === b ? 1 : 2)) return false
  for (const [h, set] of sets.entries()) {
    const anc = new Set([Math.floor(oa / 2 ** h), Math.floor(ob / 2 ** h)])
    const allowed = new Set<number>()
    for (const x of anc) {
      allowed.add(x ^ 1)
      if (h === 0) allowed.add(x)
    }
    for (const off of set.keys()) if (!allowed.has(off)) return false
    for (const x of anc) {
      if (h === 0 && set.get(x) !== true) return false
      if (!set.has(x ^ 1) && !anc.has(x ^ 1)) return false
    }
  }
  return true
}

/**
 * Whether mp proves txid with only the leaves that needs: at the lowest
 * level the txid, flagged as one, and its sibling (a hash or a duplicate),
 * and above that exactly the one sibling each level needs. It judges the
 * leaves of the levels the path has, not how many levels there are or what
 * root they give: whether the proof is true is the header check's (mined).
 */
export const minimalPath = (mp: MerklePath, txid: string): boolean => mergedPath(mp, txid, txid)

/**
 * The shape a mined token is admitted in: a BEEF V1 or V2, never an Atomic
 * BEEF, of exactly two transactions carried whole, tx and one other that an
 * input of tx spends, each proven: two minimal paths of two blocks, or one
 * merged path when both are in one block. Two separate paths of one block
 * are refused, since BEEF writers merge them. It returns the other
 * transaction's entry, the token's parent, and throws a BeefRefusal.
 *
 * BRC-95 allows in an Atomic BEEF only the subject and the ancestors needed
 * to validate its inputs, and a mined token needs none, so its parent would
 * be an unrelated transaction there.
 */
export function tokenShape(w: Wire, tx: Transaction): WireEntry {
  if (w.atomic || anyTxidOnly(w) || w.entries.length !== 2) throw new BeefRefusal('a token is a BEEF V1 or V2 of exactly the token and its parent')
  const txid = tx.id('hex')
  const self = w.byTxid.get(txid)
  const parent = w.entries.find((e) => e.txid !== txid)
  if (self === undefined || parent?.tx === undefined || self.bump < 0 || parent.bump < 0) throw new BeefRefusal('the token and its parent, each proven')
  if (!tx.inputs.some((i) => sourceTxid(i) === parent.txid)) throw new BeefRefusal('no input spends the other transaction')
  if (w.bumps.length === 2) {
    if (w.bumps[0]!.blockHeight === w.bumps[1]!.blockHeight) throw new BeefRefusal('two paths of one block, which BEEF writers merge')
    if (self.bump === parent.bump || !minimalPath(w.bumps[self.bump]!, txid) || !minimalPath(w.bumps[parent.bump]!, parent.txid)) {
      throw new BeefRefusal('two minimal paths, one each')
    }
  } else if (w.bumps.length === 1) {
    if (!mergedPath(w.bumps[0]!, txid, parent.txid)) throw new BeefRefusal('one merged path flagging exactly both txids')
  } else {
    throw new BeefRefusal(`${w.bumps.length} BUMPs`)
  }
  return parent
}

/**
 * The shape an unmined carrier is admitted in: exactly two transactions
 * carried whole and one BUMP, tx unproven and the transaction its first
 * input spends proven by that BUMP, minimally. It returns that
 * transaction's entry, the carrier's funding tree, and throws a
 * BeefRefusal.
 */
export function carrierShape(w: Wire, tx: Transaction): WireEntry {
  if (anyTxidOnly(w) || w.entries.length !== 2 || w.bumps.length !== 1) throw new BeefRefusal(`${w.bumps.length} BUMPs and ${w.entries.length} transactions`)
  const first = tx.inputs[0]
  if (first === undefined) throw new BeefRefusal('a carrier of no inputs')
  const parent = sourceTxid(first)
  const self = w.byTxid.get(tx.id('hex'))
  const fund = w.byTxid.get(parent)
  if (self === undefined || fund === undefined || self.bump !== -1 || fund.bump !== 0 || !minimalPath(w.bumps[0]!, parent)) {
    throw new BeefRefusal("not the carrier and its funding tree's minimal path")
  }
  return fund
}

/**
 * The shape a mined transaction with no parent to read is admitted in, a
 * sweep for one: exactly one transaction, tx, proven by the one BUMP, its
 * own minimal path. It throws a BeefRefusal.
 */
export function aloneShape(w: Wire, tx: Transaction): void {
  if (anyTxidOnly(w) || w.entries.length !== 1 || w.bumps.length !== 1 || w.entries[0]!.bump !== 0 || !minimalPath(w.bumps[0]!, tx.id('hex'))) {
    throw new BeefRefusal('one transaction and its own minimal path')
  }
}

/** The txid an input spends, display order. */
function sourceTxid(input: { sourceTXID?: string; sourceTransaction?: Transaction }): string {
  return input.sourceTXID ?? input.sourceTransaction?.id('hex') ?? ''
}

function writeVarInt(n: number): number[] {
  const w = new Utils.Writer()
  w.writeVarIntNum(n)
  return w.toArray()
}

/**
 * The BEEF a mined token is admitted in (tokenShape): a BEEF V1 of exactly
 * the parent then the token, each with its own proof, or one merged proof,
 * flagging both txids, when both are in one block. A replayer builds it
 * from the token as a host stores it, alone with its proof, and the parent:
 * the token before it, or for the first token of a chain the transaction
 * whose output it spends. Neither transaction is changed.
 */
export function tokenBEEF(token: Transaction, parent: Transaction): Uint8Array {
  if (token.merklePath === undefined || parent.merklePath === undefined) throw new Error("a token's BEEF needs the token and its parent, both proven")
  const tid = token.id('hex')
  const pid = parent.id('hex')
  const tp = MerklePath.fromBinary(token.merklePath.toBinary())
  const pp = MerklePath.fromBinary(parent.merklePath.toBinary())
  let bumps = [pp, tp]
  let idx = [0, 1]
  if (tp.blockHeight === pp.blockHeight && rootOf(tp, tid) !== undefined && rootOf(tp, tid) === rootOf(pp, pid)) {
    pp.combine(tp)
    // Each the other's sibling: each path lists the other's txid as a
    // plain hash. Both are flagged in the union.
    for (const e of pp.path[0] ?? []) if (e.hash === tid || e.hash === pid) e.txid = true
    bumps = [pp]
    idx = [0, 0]
  }
  const out: number[] = []
  const v = new Utils.Writer()
  v.writeUInt32LE(V1)
  out.push(...v.toArray(), ...writeVarInt(bumps.length))
  for (const m of bumps) out.push(...m.toBinary())
  out.push(...writeVarInt(2))
  for (const [i, tx] of [parent, token].entries()) {
    out.push(...tx.toBinary(), 1, ...writeVarInt(idx[i]!))
  }
  return Uint8Array.from(out)
}

function rootOf(mp: MerklePath, txid: string): string | undefined {
  try {
    return mp.computeRoot(txid)
  } catch {
    return undefined
  }
}

/**
 * A token as a host stores it: the token's transaction alone, with its
 * proof, as an overlay engine keeps a proven transaction (an Atomic BEEF,
 * or any BEEF whose subject is the token). It returns the subject
 * transaction, without a parent, and throws a BeefRefusal.
 */
export function storedToken(raw: Uint8Array, bound: number = DefaultMaxBEEF): Transaction {
  return subjectTx(readWire(raw, bound))
}

/**
 * Whether tx carries a proof that verifies against the headers. No proof
 * or no headers means nothing is mined, and a tracker that throws is one
 * that did not confirm.
 */
export async function mined(tx: Transaction | undefined, headers?: ChainTracker): Promise<boolean> {
  if (tx?.merklePath === undefined || headers === undefined) return false
  try {
    return await tx.merklePath.verify(tx.id('hex'), headers)
  } catch {
    return false
  }
}

/**
 * A PushDrop field signature: strict low-S DER (strictSignature), over
 * SHA-256 of signed, the fields concatenated, under the 33-byte key.
 */
export function verifyField(key: Uint8Array, signed: Uint8Array, der: Uint8Array): boolean {
  if (!strictSignature(der)) return false
  try {
    return verifyFieldSignature(PublicKey.fromString(hexOf(key)), Array.from(signed), Array.from(der))
  } catch {
    return false
  }
}

/** A token output read leniently: the fields its lock pushes before the signature, and the signature. */
export interface TokenOutput {
  /** The pushes before the signature: by convention the tag, then the record. */
  fields: Uint8Array[]
  /** The last push. */
  signature: Uint8Array
  /** The output's index. */
  vout: number
}

/**
 * Reads a locking script and its value as a token output of nfields fields
 * and a signature holding exactly 1 satoshi, or undefined for any other
 * shape. It reads the script leniently (pushFields) and decides nothing
 * about its encoding or its key: tokenLockedTo and tokenSignedBy do, once
 * the caller has decoded the record and derived the key it names.
 */
export function readTokenOutput(script: Uint8Array, satoshis: number | undefined, vout: number, nfields: number): TokenOutput | undefined {
  if (satoshis !== 1) return undefined
  const fields = pushFields(script)
  if (fields?.length !== nfields + 1) return undefined
  return { fields: fields.slice(0, nfields), signature: fields[nfields]!, vout }
}

/** Whether script is, byte for byte, the canonical lock of the output's fields and signature under the 33-byte key. */
export function tokenLockedTo(t: TokenOutput, script: Uint8Array, key: Uint8Array): boolean {
  return bytesEqual(script, pushDropScript(key, t.fields, t.signature))
}

/** The bytes the field signature covers: the fields concatenated. */
export function tokenSigned(t: TokenOutput): Uint8Array {
  const out = new Uint8Array(t.fields.reduce((n, f) => n + f.length, 0))
  let at = 0
  for (const f of t.fields) {
    out.set(f, at)
    at += f.length
  }
  return out
}

/** Whether the field signature is strict and verifies under the 33-byte key. */
export function tokenSignedBy(t: TokenOutput, key: Uint8Array): boolean {
  return verifyField(key, tokenSigned(t), t.signature)
}

/** One input of a token that spends an output of its parent. */
export interface TokenSpend {
  /** The index of the token's input. */
  input: number
  /** The index of the parent's output it spends. */
  vout: number
  /** That output's locking script. */
  script: Uint8Array
  /** That output's value. */
  satoshis: number | undefined
}

/**
 * The inputs of tx that spend an output of parent, in input order. An
 * input naming an output the parent does not have is left out, as is every
 * input that spends another transaction: those pay the token's fee and are
 * not read.
 */
export function tokenSpends(tx: Transaction, parent: WireEntry): TokenSpend[] {
  const out: TokenSpend[] = []
  if (parent.tx === undefined) return out
  for (const [i, input] of tx.inputs.entries()) {
    if (sourceTxid(input) !== parent.txid) continue
    const po = parent.tx.outputs[input.sourceOutputIndex]
    if (po === undefined) continue
    out.push({ input: i, vout: input.sourceOutputIndex, script: Uint8Array.from(po.lockingScript.toBinary()), satoshis: po.satoshis })
  }
  return out
}
