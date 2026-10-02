/**
 * The lenient PushDrop reader and the canonical rebuild against
 * testdata/vectors/pushdrop-v1.json: a script is canonical exactly when it
 * is the one pushDropScript rebuilds from what pushFields reads of it, under
 * its own key, which is what the Go pushdrop.Fields and Script decide.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { bytesEqual } from './cbor.js'
import { strictPublicKey } from './pubkey.js'
import { firstPush, minimalPushBytes, pushDropScript, pushFields } from './script.js'
import { fromHex, toHex } from './testing/index.js'

interface PushdropVector {
  cases: Array<{ kind: string; name: string; accept: boolean; lockHex: string }>
}

const v = JSON.parse(readFileSync(new URL('../../testdata/vectors/pushdrop-v1.json', import.meta.url), 'utf8')) as PushdropVector

/** The 33 bytes of a script's first push, whichever push holds them. */
function keyOf(s: Uint8Array): Uint8Array | undefined {
  if (s.length >= 34 && s[0] === 33) return s.subarray(1, 34)
  if (s.length >= 35 && s[0] === 0x4c && s[1] === 33) return s.subarray(2, 35)
  return undefined
}

test('the rebuild decides what the canonical check decides, for every case of the vector', () => {
  assert.ok(v.cases.length >= 20, `${v.cases.length} cases`)
  for (const c of v.cases) {
    const s = fromHex(c.lockHex)
    const fields = pushFields(s)
    const key = keyOf(s)
    const rebuilt = fields !== undefined && key !== undefined && strictPublicKey(key) !== undefined && bytesEqual(s, pushDropScript(key, fields))
    assert.equal(rebuilt, c.accept, `${c.kind}/${c.name}`)
  }
})

const key = Uint8Array.from([2, ...new Array<number>(32).fill(2)])
const head = Uint8Array.from([33, ...key, 0xac])
const withTail = (...tail: number[]): Uint8Array => Uint8Array.from([...head, ...tail])
const big = new Uint8Array(300).fill(9)

test('firstPush reads the one push after the key and OP_CHECKSIG', () => {
  const cases: Array<[string, Uint8Array, Uint8Array]> = [
    ['a direct push', withTail(3, 0x61, 0x62, 0x63), Uint8Array.of(0x61, 0x62, 0x63)],
    ['a direct push with more after it', withTail(1, 0x61, 0x75), Uint8Array.of(0x61)],
    ['OP_PUSHDATA1', withTail(0x4c, 2, 0x61, 0x62), Uint8Array.of(0x61, 0x62)],
    ['OP_PUSHDATA1 of nothing', withTail(0x4c, 0), new Uint8Array(0)],
    ['OP_PUSHDATA2', withTail(0x4d, 300 & 0xff, 300 >> 8, ...big), big],
    ['OP_PUSHDATA4', withTail(0x4e, 300 & 0xff, 300 >> 8, 0, 0, ...big), big],
  ]
  for (const [name, s, want] of cases) {
    const got = firstPush(s)
    assert.ok(got !== undefined && bytesEqual(got, want), name)
  }
  const refused: Array<[string, Uint8Array]> = [
    ['nothing', new Uint8Array(0)],
    ['the key and OP_CHECKSIG alone', head],
    ['OP_0 after OP_CHECKSIG', withTail(0x00)],
    ['OP_5 after OP_CHECKSIG', withTail(0x55)],
    ['OP_DROP after OP_CHECKSIG', withTail(0x75)],
    ['a direct push one byte short', withTail(3, 0x61, 0x62)],
    ['OP_PUSHDATA1 with no length', withTail(0x4c)],
    ['OP_PUSHDATA1 one byte short', withTail(0x4c, 2, 0x61)],
    ['OP_PUSHDATA2 with half a length', withTail(0x4d, 1)],
    ['OP_PUSHDATA2 short', withTail(0x4d, 5, 0, 0x61)],
    ['OP_PUSHDATA4 declaring 2^32-1 bytes', withTail(0x4e, 0xff, 0xff, 0xff, 0xff, 0x61)],
    ['OP_PUSHDATA4 short', withTail(0x4e, 5, 0, 0, 0, 0x61)],
    ['a 32-byte key', Uint8Array.from([32, ...key.subarray(0, 32), 0xac, 1, 0x61, 0])],
    ['no OP_CHECKSIG after the key', Uint8Array.from([33, ...key, 0x75, 1, 0x61])],
    ['the key pushed with OP_PUSHDATA1', Uint8Array.from([0x4c, 33, ...key, 0xac, 1, 0x61])],
  ]
  for (const [name, s] of refused) assert.equal(firstPush(s), undefined, name)
})

test('pushFields stops at the first opcode that is not a data push, and refuses a truncated script whole', () => {
  const got = pushFields(withTail(1, 0x61, 0x00, 0x4c, 1, 0x62, 0x6d, 0x75, 1, 0x7a))
  assert.deepEqual(got?.map(toHex), ['61', '', '62'])
  const refused: Array<[string, Uint8Array]> = [
    ['nothing', new Uint8Array(0)],
    ['the key and OP_CHECKSIG alone', head],
    ['an opcode where the first field', withTail(0x75, 1, 0x61)],
    ['a push past the end, later on', withTail(1, 0x61, 0x75, 5, 0x78)],
    ['OP_PUSHDATA4 past the end', withTail(0x4e, 9, 0, 0, 0, 0x78)],
    ['a 32-byte first push', Uint8Array.from([32, ...key.subarray(0, 32), 0xac, 1, 0x61])],
    ['no OP_CHECKSIG', Uint8Array.from([33, ...key, 0x75, 1, 0x61])],
  ]
  for (const [name, s] of refused) assert.equal(pushFields(s), undefined, name)
  // OP_1 to OP_16 are not data pushes to this reader.
  assert.equal(pushFields(withTail(1, 0x61, 0x55, 1, 0x62))?.length, 1)
})

test('pushDropScript writes each field in its minimal push and the fewest drops', () => {
  const sizes = [0, 1, 2, 16, 33, 74, 75, 76, 77, 254, 255, 256, 257, 65535, 65536, 65537]
  for (const n of sizes) {
    const f = new Uint8Array(n).fill(0x5a)
    const p = minimalPushBytes(f)
    const headLen = n === 0 ? 1 : n <= 75 ? 1 : n <= 255 ? 2 : n <= 65535 ? 3 : 5
    assert.equal(p.length, (n === 0 ? 0 : n) + headLen, `${n} bytes`)
    const s = pushDropScript(key, [Uint8Array.of(0x76, 0x78), f])
    const read = pushFields(s)
    assert.ok(read !== undefined && read.length === 2 && bytesEqual(read[1]!, f), `${n} bytes read back`)
    assert.deepEqual(Array.from(s.subarray(s.length - 1)), [0x6d], `${n} bytes: two fields, one OP_2DROP`)
  }
  for (const [b, op] of [
    [0x00, 0x00],
    [0x01, 0x51],
    [0x10, 0x60],
    [0x81, 0x4f],
  ] as Array<[number, number]>) {
    assert.deepEqual(Array.from(minimalPushBytes(Uint8Array.of(b))), [op], `0x${b.toString(16)}`)
  }
  assert.deepEqual(Array.from(minimalPushBytes(Uint8Array.of(0x11))), [1, 0x11])
  const one = pushDropScript(key, [Uint8Array.of(0x76)])
  assert.equal(one[one.length - 1], 0x75)
  const three = pushDropScript(key, [Uint8Array.of(0x76), Uint8Array.of(0x77)], Uint8Array.of(0x78))
  assert.deepEqual(Array.from(three.subarray(three.length - 2)), [0x6d, 0x75])
  // An empty signature is still a signature: it is pushed.
  assert.notDeepEqual(pushDropScript(key, [Uint8Array.of(0x76)], new Uint8Array(0)), pushDropScript(key, [Uint8Array.of(0x76)]))
})
