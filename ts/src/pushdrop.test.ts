/**
 * decodeStrictPushDrop, strictPublicKey and readerLockingKey on the forms the
 * vectors have no case for, as the Go pushdrop and guard tests hold them.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { OP, PrivateKey, Script } from '@bsv/sdk'
import { readerLockingKey } from './derive.js'
import { strictPublicKey, strictPublicKeyHex } from './pubkey.js'
import { decodeStrictPushDrop } from './pushdrop.js'

const x1 = '02' + '00'.repeat(31) + '01'
const alias = '02fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc30'

/** A lock-before PushDrop of a field OP_1 and then field to the x = 1 key. */
function lockWith(field: number[]): Script {
  const key = [0x02, ...new Array<number>(31).fill(0), 1]
  return Script.fromBinary([33, ...key, OP.OP_CHECKSIG, OP.OP_1, ...field, OP.OP_2DROP])
}

test('decodeStrictPushDrop holds each push to its one minimal form', () => {
  const b = (n: number): number[] => new Array<number>(n).fill(7)
  const direct = (d: number[]): number[] => [d.length, ...d]
  const pd1 = (d: number[]): number[] => [OP.OP_PUSHDATA1, d.length, ...d]
  const pd2 = (d: number[]): number[] => [OP.OP_PUSHDATA2, d.length & 0xff, d.length >> 8, ...d]
  const cases: Array<[string, boolean, number[]]> = [
    ['OP_0', true, [OP.OP_0]],
    ['OP_5', true, [OP.OP_5]],
    ['OP_16', true, [OP.OP_16]],
    ['OP_1NEGATE', true, [OP.OP_1NEGATE]],
    ['0x11 as a direct push, above OP_16', true, direct([0x11])],
    ['75 bytes direct', true, direct(b(75))],
    ['76 bytes with OP_PUSHDATA1', true, pd1(b(76))],
    ['256 bytes with OP_PUSHDATA2', true, pd2(b(256))],
    ['0x05 as a direct push rather than OP_5', false, direct([0x05])],
    ['0x00 as a direct push rather than OP_0', false, direct([0x00])],
    ['0x81 as a direct push rather than OP_1NEGATE', false, direct([0x81])],
    ['an empty OP_PUSHDATA1 rather than OP_0', false, pd1([])],
    ['75 bytes with OP_PUSHDATA1', false, pd1(b(75))],
    ['76 bytes with OP_PUSHDATA2', false, pd2(b(76))],
    ['OP_NOP', false, [OP.OP_NOP]],
  ]
  for (const [name, accept, field] of cases) {
    assert.equal(decodeStrictPushDrop(lockWith(field)) !== undefined, accept, name)
  }
})

test('strictPublicKey takes a key only in its canonical compressed encoding', () => {
  assert.equal(strictPublicKeyHex(x1)?.toString(), x1)
  assert.equal(strictPublicKeyHex(alias), undefined)
  const test42 = PrivateKey.fromHex('42'.repeat(32)).toPublicKey().toString()
  assert.equal(strictPublicKeyHex(test42)?.toString(), test42)
  assert.equal(strictPublicKeyHex(test42.toUpperCase())?.toString(), test42, 'upper-case hex is the same key')
  assert.equal(strictPublicKeyHex(alias.toUpperCase()), undefined, 'the alias in upper case')
  assert.equal(strictPublicKeyHex(x1.slice(0, 64)), undefined, 'short')
  assert.equal(strictPublicKey([]), undefined, 'empty')
  assert.equal(strictPublicKey([0x02, ...new Array<number>(31).fill(0), 5]), undefined, 'x = 5, off the curve')
})

test('readerLockingKey refuses an identity that is not a canonical key, though the SDK would derive from it', () => {
  const protocol: [0, string] = [0, 'vector sample']
  assert.ok(readerLockingKey(protocol, 'object', x1))
  assert.throws(() => readerLockingKey(protocol, 'object', alias))
})
