/**
 * The record reader against testdata/vectors/record-v1.json, the vector the
 * Go package record reads: every case is accepted or refused for the same
 * reason in both languages, and an accepted record re-encodes to the bytes
 * it was read from.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { type Pair, type Value } from './cbor.js'
import { MaxKeys, ascending, claimsRecord, recordReader, type RecordReason } from './record.js'
import { fromHex, toHex } from './testing/index.js'

interface Field {
  key: number
  kind: string
  lo?: number
  hi?: number
  n?: number
  max?: number
  optional?: boolean
}

interface RecordVector {
  magicHex: string
  last: number
  max: number
  maxKeys: number
  plan: Field[]
  encodedHex: string
  cases: Array<{ name: string; reason: string; extra: number; hex: string }>
  claims: Array<{ name: string; claims: boolean; hex: string }>
}

const v = JSON.parse(readFileSync(new URL('../../testdata/vectors/record-v1.json', import.meta.url), 'utf8')) as RecordVector
const magic = fromHex(v.magicHex)

/** An application's own refusal, as the reader is asked to throw it. */
class Refusal extends Error {
  constructor(
    readonly reason: RecordReason,
    detail?: string,
  ) {
    super(detail === undefined ? `sample: ${reason}` : `sample: ${reason}: ${detail}`)
  }
}

const rec = recordReader((reason, detail) => new Refusal(reason, detail))

/** Follows the vector's plan over b, as an application's decoder reads its own keys. */
function read(b: Uint8Array, viaSteps = false): { pairs: Pair[]; extra: Pair[] } {
  let f
  if (viaSteps) {
    f = rec.split(rec.decodeMap(b, v.max), BigInt(v.last))
    rec.checkMagic(f, magic)
  } else {
    f = rec.decode(b, v.max, v.last, magic)
  }
  const pairs: Pair[] = [{ key: 0n, val: magic }]
  for (const p of v.plan) {
    if (p.optional === true && !f.known.has(p.key)) continue
    let val: Value
    switch (p.kind) {
      case 'text':
        val = rec.text(f, p.key)
        break
      case 'uint':
        val = BigInt(rec.uint(f, p.key, p.lo ?? 0, p.hi ?? 0))
        break
      case 'bytes':
        val = rec.bytes(f, p.key)
        break
      case 'bytesN':
        val = rec.bytesN(f, p.key, p.n ?? 0)
        break
      case 'bytesRange':
        val = rec.bytesRange(f, p.key, p.lo ?? 0, p.hi ?? 0)
        break
      case 'bool':
        val = rec.bool(f, p.key)
        break
      case 'list32':
        val = rec.list32(f, p.key, p.max ?? 0)
        break
      default:
        throw new Error(`unknown kind ${p.kind}`)
    }
    pairs.push({ key: BigInt(p.key), val })
  }
  return { pairs, extra: f.extra }
}

function reasonOf(f: () => unknown): string {
  try {
    f()
  } catch (e) {
    if (e instanceof Refusal) return e.reason
    throw e
  }
  return ''
}

test('the vector is the one this test was written for', () => {
  assert.equal(v.maxKeys, MaxKeys)
  assert.ok(v.cases.length >= 40, `${v.cases.length} cases`)
  assert.ok(v.claims.length >= 12, `${v.claims.length} claims`)
  assert.equal(v.cases[0]!.hex, v.encodedHex)
})

test('every case is accepted or refused for the reason the vector names', () => {
  let accepted = 0
  for (const c of v.cases) {
    const b = fromHex(c.hex)
    assert.equal(
      reasonOf(() => read(b)),
      c.reason,
      c.name,
    )
    assert.equal(
      reasonOf(() => read(b, true)),
      c.reason,
      `${c.name}, one step at a time`,
    )
    if (c.reason !== '') continue
    accepted++
    const { pairs, extra } = read(b)
    assert.equal(extra.length, c.extra, c.name)
    rec.checkExtra(extra, v.last, pairs.length)
    assert.equal(toHex(rec.encode(pairs, extra, v.max)), c.hex, `${c.name}: re-encoded`)
  }
  assert.ok(accepted >= 8, `${accepted} accepted cases`)
})

test('a refusal is the error the application supplied, with its detail', () => {
  const missing = v.cases.find((c) => c.name === 'key 1 missing')!
  assert.throws(
    () => read(fromHex(missing.hex)),
    (e: unknown) => e instanceof Refusal && e.reason === 'missing' && e.message === 'sample: missing: key 1',
  )
  assert.throws(
    () => read(fromHex('ff')),
    (e: unknown) => e instanceof Refusal && e.reason === 'cbor' && e.message.startsWith('sample: cbor: '),
  )
})

test('claimsRecord reads only the head', () => {
  for (const c of v.claims) assert.equal(claimsRecord(fromHex(c.hex), magic), c.claims, c.name)
})

test('preserved pairs are held to keys above the last defined one, and to the bound on entries', () => {
  const ok: Pair[] = [{ key: 8n, val: 1n }]
  rec.checkExtraKeys(ok, 7)
  rec.checkExtraKeys(ok, 7n)
  for (const [name, extra] of Object.entries({
    'a defined key': [{ key: 7n, val: 1n }],
    'key 0': [{ key: 0n, val: magic }],
    'a text key': [{ key: 'a', val: 1n }],
    'a negative key': [{ key: -1n, val: 1n }],
    'one bad of many': [
      { key: 9n, val: 1n },
      { key: 3n, val: 1n },
    ],
  } as Record<string, Pair[]>)) {
    assert.equal(
      reasonOf(() => rec.checkExtraKeys(extra, 7)),
      'key-type',
      name,
    )
    assert.equal(
      reasonOf(() => rec.checkExtra(extra, 7, 8)),
      'key-type',
      name,
    )
  }
  const many: Pair[] = []
  for (let k = 8n; many.length < MaxKeys - 8; k++) many.push({ key: k, val: k })
  rec.checkExtra(many, 7, 8)
  assert.equal(
    reasonOf(() => rec.checkExtra(many, 7, 9)),
    'too-large',
  )
  rec.checkExtraKeys([...many, { key: 1000n, val: 0n }], 7)
})

test('encode holds its output to the bound', () => {
  const pairs: Pair[] = [
    { key: 1n, val: 'one' },
    { key: 0n, val: magic },
  ]
  const out = rec.encode(pairs, [{ key: 9n, val: 9n }], 64)
  assert.equal(rec.decode(out, 64, 1, magic).extra.length, 1)
  rec.encode(pairs, [{ key: 9n, val: 9n }], out.length)
  assert.equal(
    reasonOf(() => rec.encode(pairs, [{ key: 9n, val: 9n }], out.length - 1)),
    'too-large',
  )
})

test('inRange takes safe integers within its bounds', () => {
  rec.inRange(5, 5, 5, 'x')
  for (const n of [4, 6, 5.5, Number.MAX_SAFE_INTEGER + 1, Number.NaN]) {
    assert.equal(
      reasonOf(() => rec.inRange(n, 5, 5, 'x')),
      'range',
      String(n),
    )
  }
})

test('ascending is strict', () => {
  const a = Uint8Array.of(1)
  const b = Uint8Array.of(2)
  assert.ok(ascending([]))
  assert.ok(ascending([a]))
  assert.ok(ascending([a, b]))
  assert.ok(!ascending([a, a]))
  assert.ok(!ascending([b, a]))
})
