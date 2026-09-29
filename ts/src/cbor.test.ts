/**
 * The codec against the Go package's own cases (cbor/cbor_test.go), so both
 * sides accept exactly the same canonical subset and refuse the same things.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { CborError, CborMap, MaxDepth, decodeValue, encode, type Value } from './cbor.js'

const bytes = (...b: number[]): Uint8Array => Uint8Array.from(b)

function refuses(b: Uint8Array, code: CborError['code'], what: string): void {
  assert.throws(
    () => decodeValue(b),
    (err: unknown) => err instanceof CborError && err.code === code,
    `${what}: expected ${code}`,
  )
}

test('integers encode in shortest form and round trip', () => {
  const cases: Array<[Value, number[]]> = [
    [0n, [0x00]],
    [23n, [0x17]],
    [24n, [0x18, 0x18]],
    [255n, [0x18, 0xff]],
    [256n, [0x19, 0x01, 0x00]],
    [65535n, [0x19, 0xff, 0xff]],
    [65536n, [0x1a, 0x00, 0x01, 0x00, 0x00]],
    [4294967295n, [0x1a, 0xff, 0xff, 0xff, 0xff]],
    [4294967296n, [0x1b, 0, 0, 0, 1, 0, 0, 0, 0]],
    [(1n << 64n) - 1n, [0x1b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff]],
    [-1n, [0x20]],
    [-25n, [0x38, 0x18]],
    [7n, [0x07]],
  ]
  for (const [v, want] of cases) {
    const got = encode(v)
    assert.deepEqual([...got], want, `encode ${String(v)}`)
    assert.equal(decodeValue(got), v, `round trip ${String(v)}`)
  }
  assert.throws(() => encode(1n << 64n), (e: unknown) => e instanceof CborError && e.code === 'unsupported')
})

test('non-shortest argument encodings are refused', () => {
  for (const b of [
    bytes(0x18, 0x05), // 5 in two bytes
    bytes(0x19, 0x00, 0xff), // 255 in three
    bytes(0x1a, 0x00, 0x00, 0xff, 0xff), // 65535 in five
    bytes(0x1b, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff),
    bytes(0x58, 0x01, 0xaa), // a 1-byte byte string whose length takes two bytes
  ]) {
    refuses(b, 'not-canonical', Buffer.from(b).toString('hex'))
  }
})

test('strings and bytes', () => {
  assert.deepEqual([...encode('a')], [0x61, 0x61])
  assert.deepEqual([...encode(bytes(1, 2))], [0x42, 1, 2])
  // A lone surrogate is not UTF-8; Go's utf8.ValidString refuses it and so
  // does this, rather than letting TextEncoder replace it silently.
  assert.throws(() => encode('\ud800'), (e: unknown) => e instanceof CborError && e.code === 'utf8')
  refuses(bytes(0x62, 0xff, 0xfe), 'utf8', 'invalid utf8 text')
  // Overlong and surrogate encodings are invalid UTF-8 too.
  refuses(bytes(0x62, 0xc0, 0x80), 'utf8', 'overlong')
  refuses(bytes(0x63, 0xed, 0xa0, 0x80), 'utf8', 'encoded surrogate')
  const v = decodeValue(bytes(0x42, 1, 2))
  assert.ok(v instanceof Uint8Array)
  assert.deepEqual([...v], [1, 2])
  // A leading BOM survives a round trip: it is a character like any other.
  const bom = '﻿x'
  assert.equal(decodeValue(encode(bom)), bom)
  // Multi-byte text lengths are byte lengths.
  assert.deepEqual([...encode('é')], [0x62, 0xc3, 0xa9])
})

test('maps are sorted and unique', () => {
  const got = encode(
    new CborMap([
      { key: 'b', val: 1n },
      { key: 'a', val: 2n },
      { key: 3n, val: true },
    ]),
  )
  // integer key 3 (0x03) sorts before text keys (0x61..); "a" before "b".
  const want = [0xa3, 0x03, 0xf5, 0x61, 0x61, 0x02, 0x61, 0x62, 0x01]
  assert.deepEqual([...got], want)
  assert.throws(
    () =>
      encode(
        new CborMap([
          { key: 'a', val: 1n },
          { key: 'a', val: 2n },
        ]),
      ),
    (e: unknown) => e instanceof CborError && e.code === 'duplicate-key',
  )
  refuses(bytes(0xa2, 0x61, 0x62, 0x01, 0x61, 0x61, 0x02), 'key-order', 'unsorted map')
  refuses(bytes(0xa2, 0x61, 0x61, 0x01, 0x61, 0x61, 0x02), 'duplicate-key', 'duplicate map key')
  const m = decodeValue(Uint8Array.from(want))
  assert.ok(m instanceof CborMap)
  assert.equal(m.get('a'), 2n)
  assert.equal(m.get(3n), true)
  assert.equal(m.get('zzz'), undefined)
  // Wire order is preserved on decode, not re-sorted or keyed.
  assert.deepEqual(
    m.entries.map((p) => p.key),
    [3n, 'a', 'b'],
  )
})

test('items outside the subset are refused', () => {
  const cases: Array<[string, number[], CborError['code']]> = [
    ['indefinite array', [0x9f, 0xff], 'unsupported'],
    ['indefinite bstr', [0x5f, 0xff], 'unsupported'],
    ['tag', [0xc0, 0x00], 'unsupported'],
    ['float16', [0xf9, 0x3c, 0x00], 'unsupported'],
    ['float64', [0xfb, 0, 0, 0, 0, 0, 0, 0, 0], 'unsupported'],
    ['undefined', [0xf7], 'unsupported'],
    ['simple 32', [0xf8, 0x20], 'unsupported'],
    ['reserved ai 28', [0x1c], 'unsupported'],
    ['trailing', [0x01, 0x02], 'trailing'],
    ['truncated bstr', [0x45, 1, 2], 'truncated'],
    ['truncated array', [0x83, 0x01], 'truncated'],
    ['huge array count', [0x1b | 0x80, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff], 'truncated'],
    ['huge map count', [0x1a | 0xa0, 0x00, 0x01, 0x00, 0x00, 0x00], 'truncated'],
    ['empty', [], 'truncated'],
    ['negative overflow', [0x3b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff], 'unsupported'],
    ['map count beyond half the remaining bytes', [0xa2, 0x01, 0x02], 'truncated'],
  ]
  for (const [name, b, code] of cases) refuses(Uint8Array.from(b), code, name)

  // Nesting one deeper than MaxDepth is refused; exactly MaxDepth is fine.
  const deep: number[] = []
  for (let i = 0; i <= MaxDepth; i++) deep.push(0x81)
  deep.push(0x00)
  refuses(Uint8Array.from(deep), 'depth', 'nested too deep')
  const ok: number[] = []
  for (let i = 0; i < MaxDepth; i++) ok.push(0x81)
  ok.push(0x00)
  assert.doesNotThrow(() => decodeValue(Uint8Array.from(ok)))

  let v: Value = 0n
  for (let i = 0; i <= MaxDepth; i++) v = [v]
  assert.throws(() => encode(v), (e: unknown) => e instanceof CborError && e.code === 'depth')
  assert.throws(() => encode(3.5 as unknown as Value), (e: unknown) => e instanceof CborError && e.code === 'unsupported')
  assert.throws(() => encode(1 as unknown as Value), (e: unknown) => e instanceof CborError && e.code === 'unsupported')
})

test('simple values', () => {
  for (const [v, b] of [
    [false, [0xf4]],
    [true, [0xf5]],
    [null, [0xf6]],
  ] as Array<[Value, number[]]>) {
    assert.deepEqual([...encode(v)], b)
    assert.equal(decodeValue(Uint8Array.from(b)), v)
  }
})

test('a generic value round trips to identical bytes', () => {
  const v = new CborMap([
    { key: 1n, val: bytes(9, 9, 9) },
    { key: 'nested', val: new CborMap([{ key: 'list', val: [1n, -2n, 'three', null, true] }]) },
  ])
  const b = encode(v)
  const back = decodeValue(b)
  const again = encode(back)
  assert.deepEqual([...again], [...b], 'not stable')
})
