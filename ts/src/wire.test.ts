/**
 * The wire BEEF reader, its shapes, the replayer's BEEF and the token
 * output against testdata/vectors/chaintoken-v1.json, the vector the Go
 * package chaintoken reads, and the structural walk against beef-v1.json,
 * the vector the Go guard reads: both languages give every case the same
 * verdict.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { type ChainTracker } from '@bsv/sdk'
import { strictSignature } from './carrier.js'
import { fromHex, toHex } from './testing/index.js'
import {
  BeefRefusal,
  DefaultMaxBEEF,
  aloneShape,
  carrierShape,
  checkBEEF,
  mined,
  readTokenOutput,
  readWire,
  storedToken,
  subjectTx,
  tokenBEEF,
  tokenLockedTo,
  tokenShape,
  tokenSigned,
  tokenSignedBy,
  tokenSpends,
  type WireEntry,
} from './wire.js'

function vector<T>(name: string): T {
  return JSON.parse(readFileSync(new URL(`../../testdata/vectors/${name}`, import.meta.url), 'utf8')) as T
}

interface TokenVector {
  stateLockingKeyHex: string
  blocks: Array<{ height: number; rootHex: string }>
  shapes: Array<{
    name: string
    shape: 'token' | 'carrier' | 'alone'
    reads: boolean
    accept: boolean
    subjectTxid?: string
    parentTxid?: string
    spends?: Array<{ input: number; vout: number }>
    mined?: boolean
    beefHex: string
  }>
  replays: Array<{ name: string; tokenStoredHex: string; parentStoredHex: string; tokenTxid: string; parentTxid: string; beefHex: string }>
  outputs: Array<{
    name: string
    satoshis: number
    fields: number
    scriptHex: string
    reads: boolean
    fieldsHex?: string[]
    signatureHex?: string
    locked: boolean
    signed: boolean
  }>
  signatures: Array<{ name: string; strict: boolean; hex: string }>
}

const v = vector<TokenVector>('chaintoken-v1.json')

// The SDK reads a txid at offset 0 as a coinbase and asks for 100 blocks
// over it, so the tip is far above every block of the vector.
const roots = new Map(v.blocks.map((b) => [b.height, b.rootHex]))
const tracker: ChainTracker = {
  isValidRootForHeight: async (root: string, height: number) => roots.get(height) === root,
  currentHeight: async () => 100000,
}

function refused(f: () => unknown): boolean {
  try {
    f()
  } catch (e) {
    assert.ok(e instanceof BeefRefusal, `refused with ${String(e)}, want a BeefRefusal`)
    return true
  }
  return false
}

test('every BEEF reads or not, names its subject, and is exactly what its shape needs or not', async () => {
  assert.ok(v.shapes.length >= 45, `${v.shapes.length} shape cases`)
  const accepted = { token: 0, carrier: 0, alone: 0 }
  for (const c of v.shapes) {
    const raw = fromHex(c.beefHex)
    // A case the vector refuses may be refused here at the read: the SDK
    // refuses some paths that the Go reader reads and the shape then
    // refuses. Either way it is refused as a BEEF.
    const read = !refused(() => readWire(raw))
    if (!c.reads || c.accept) assert.equal(read, c.reads, `${c.name}: reads`)
    if (!read) continue
    const w = readWire(raw)
    const tx = subjectTx(w)
    assert.equal(tx.id('hex'), c.subjectTxid, `${c.name}: subject`)
    let parent: WireEntry | undefined
    const ok = !refused(() => {
      if (c.shape === 'token') parent = tokenShape(w, tx)
      else if (c.shape === 'carrier') parent = carrierShape(w, tx)
      else aloneShape(w, tx)
    })
    assert.equal(ok, c.accept, `${c.name}: accepted`)
    if (!ok) continue
    accepted[c.shape]++
    if (c.shape === 'alone') continue
    assert.equal(parent?.txid, c.parentTxid, `${c.name}: parent`)
    assert.ok(parent?.tx !== undefined, `${c.name}: the parent is carried whole`)
    if (c.shape !== 'token') continue
    assert.deepEqual(
      tokenSpends(tx, parent).map((s) => ({ input: s.input, vout: s.vout })),
      c.spends,
      `${c.name}: spends`,
    )
    assert.equal((await mined(tx, tracker)) && (await mined(parent.tx, tracker)), c.mined, `${c.name}: mined`)
  }
  assert.ok(accepted.token >= 8 && accepted.carrier >= 3 && accepted.alone >= 2, JSON.stringify(accepted))
})

test("a replayer's BEEF is the vector's, byte for byte, and reads back as a token and its parent", async () => {
  assert.ok(v.replays.length >= 4)
  for (const c of v.replays) {
    const token = storedToken(fromHex(c.tokenStoredHex))
    const parent = storedToken(fromHex(c.parentStoredHex))
    assert.equal(token.id('hex'), c.tokenTxid, c.name)
    assert.equal(parent.id('hex'), c.parentTxid, c.name)
    const before = [toHex(token.merklePath!.toBinary()), toHex(parent.merklePath!.toBinary())]
    const beef = tokenBEEF(token, parent)
    assert.equal(toHex(beef), c.beefHex, c.name)
    assert.deepEqual([toHex(token.merklePath!.toBinary()), toHex(parent.merklePath!.toBinary())], before, `${c.name}: the stored paths are unchanged`)
    const w = readWire(beef)
    const p = tokenShape(w, subjectTx(w))
    assert.equal(p.txid, c.parentTxid, c.name)
    assert.ok((await mined(subjectTx(w), tracker)) && (await mined(p.tx, tracker)), `${c.name}: both proofs verify`)
  }
})

test('a token output is read, and held to its key and its signature', () => {
  const key = fromHex(v.stateLockingKeyHex)
  assert.ok(v.outputs.length >= 18)
  for (const c of v.outputs) {
    const s = fromHex(c.scriptHex)
    const out = readTokenOutput(s, c.satoshis, 3, c.fields)
    assert.equal(out !== undefined, c.reads, `${c.name}: reads`)
    if (out === undefined) continue
    assert.equal(out.vout, 3)
    assert.deepEqual(out.fields.map(toHex), c.fieldsHex, `${c.name}: fields`)
    assert.equal(toHex(out.signature), c.signatureHex, `${c.name}: signature`)
    assert.equal(toHex(tokenSigned(out)), (c.fieldsHex ?? []).join(''), `${c.name}: signed bytes`)
    assert.equal(tokenLockedTo(out, s, key), c.locked, `${c.name}: locked`)
    assert.equal(tokenSignedBy(out, key), c.signed, `${c.name}: signed`)
  }
  assert.equal(readTokenOutput(fromHex(v.outputs[0]!.scriptHex), undefined, 0, 2), undefined)
})

test('a signature is strict or not as the vector says', () => {
  assert.ok(v.signatures.length >= 10)
  for (const c of v.signatures) assert.equal(strictSignature(fromHex(c.hex)), c.strict, c.name)
})

test('mined answers false for everything that is not a proven transaction and a header source', async () => {
  const tx = storedToken(fromHex(v.replays[0]!.tokenStoredHex))
  assert.equal(await mined(tx, tracker), true)
  assert.equal(await mined(undefined, tracker), false)
  assert.equal(await mined(tx, undefined), false)
  const throwing: ChainTracker = {
    isValidRootForHeight: async () => {
      throw new Error('unreachable')
    },
    currentHeight: async () => 100000,
  }
  assert.equal(await mined(tx, throwing), false)
  const other: ChainTracker = { isValidRootForHeight: async () => false, currentHeight: async () => 100000 }
  assert.equal(await mined(tx, other), false)
  const bare = storedToken(fromHex(v.replays[0]!.tokenStoredHex))
  bare.merklePath = undefined
  assert.equal(await mined(bare, tracker), false)
})

test('the bound is the caller’s', () => {
  const b = fromHex(v.shapes[0]!.beefHex)
  readWire(b, b.length)
  assert.ok(refused(() => readWire(b, b.length - 1)))
  assert.ok(refused(() => storedToken(b, b.length - 1)))
  assert.ok(refused(() => readWire(new Uint8Array(DefaultMaxBEEF + 1))))
})

interface BeefVector {
  cases: Array<{ name: string; accept: boolean; beefHex: string }>
}

test('the structural walk admits and refuses what the Go guard does', () => {
  const g = vector<BeefVector>('beef-v1.json')
  assert.ok(g.cases.length >= 20, `${g.cases.length} cases`)
  let refusedCases = 0
  for (const c of g.cases) {
    const raw = fromHex(c.beefHex)
    const why = checkBEEF(raw, 64 << 20)
    assert.equal(why === undefined, c.accept, `${c.name}: ${why ?? 'admitted'}`)
    if (!c.accept) {
      refusedCases++
      assert.ok(refused(() => readWire(raw, 64 << 20)), `${c.name}: readWire reads what the walk refuses`)
    }
  }
  assert.ok(refusedCases >= 15)
})

test('the walk survives every truncation and every single-byte change of a BEEF', () => {
  const b = fromHex(v.shapes[0]!.beefHex)
  for (let n = 0; n < b.length; n++) {
    assert.notEqual(checkBEEF(b.subarray(0, n), DefaultMaxBEEF), undefined, `cut at ${n}`)
  }
  for (let i = 0; i < b.length; i++) {
    const m = Uint8Array.from(b)
    m[i] = m[i]! ^ 0xff
    // Whatever it now is, reading it either works or is refused as a
    // BEEF, and never throws anything else or runs away.
    refused(() => readWire(m))
  }
})
