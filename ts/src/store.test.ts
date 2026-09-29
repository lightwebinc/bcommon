/**
 * The refs-entry codec on its own, with neutral store names: the round trip
 * with head and an unknown member, the bounds the Go codec holds, and the
 * refusal codes and details an application maps onto its own error.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { CborMap, decodeValue, encode, type Value } from './cbor.js'
import { MaxRefMembers, MaxRefName, MaxRefs, StoreError, decodeRefs, encodeRefs, type Ref } from './store.js'

const fill = (b: number, n = 32): Uint8Array => new Uint8Array(n).fill(b)
const hex = (b: Uint8Array): string => Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('')

/** The error f throws, which must be a StoreError. */
function refused(f: () => unknown): StoreError {
  try {
    f()
  } catch (err) {
    assert.ok(err instanceof StoreError, `not a StoreError: ${String(err)}`)
    return err
  }
  assert.fail('not refused')
}

test('the bounds match the Go codec', () => {
  assert.equal(MaxRefs, 64)
  assert.equal(MaxRefMembers, 8)
  assert.equal(MaxRefName, 64)
})

test('entries round trip through their CBOR bytes, head and unknown members kept', () => {
  const refs: Ref[] = [
    { name: 'photos', root: fill(0xaa), count: 3n, head: fill(0xcc) },
    { name: 'notes', root: fill(0xbb), count: 1n, unknown: [{ key: 'salt', val: fill(0xee, 16) }] },
  ]
  const bytes = encode(encodeRefs(refs))
  const back = decodeRefs(decodeValue(bytes))
  assert.equal(back.length, 2)
  assert.equal(back[0]!.name, 'photos')
  assert.equal(hex(back[0]!.root), hex(fill(0xaa)))
  assert.equal(back[0]!.count, 3n)
  assert.equal(hex(back[0]!.head!), hex(fill(0xcc)))
  assert.equal(back[0]!.unknown, undefined)
  assert.equal(back[1]!.head, undefined)
  assert.equal(back[1]!.unknown?.[0]?.key, 'salt')
  assert.equal(hex(back[1]!.unknown![0]!.val as Uint8Array), hex(fill(0xee, 16)))
  assert.equal(hex(encode(encodeRefs(back))), hex(bytes), 'a re-encode is faithful')
  assert.deepEqual(encodeRefs([]), [])
})

test('the refusals carry their code and detail', () => {
  const ref = (name: string): Ref => ({ name, root: fill(0xaa), count: 1n })
  // One decoded entry named a, with the root, count or head given.
  const decoded = (m: { root?: Value; count?: Value; head?: Value }) =>
    decodeRefs([
      new CborMap([
        { key: 'name', val: 'a' },
        { key: 'root', val: m.root ?? fill(0xaa) },
        { key: 'count', val: m.count ?? 1n },
        ...(m.head === undefined ? [] : [{ key: 'head', val: m.head }]),
      ]),
    ])
  const cases: Array<[string, () => unknown, string]> = [
    ['empty name', () => encodeRefs([ref('')]), 'store: field: ref name'],
    ['long name', () => encodeRefs([ref('é'.repeat(MaxRefName / 2 + 1))]), 'store: field: ref name'],
    ['duplicate', () => encodeRefs([ref('a'), ref('a')]), 'store: dup-ref: a'],
    ['short root', () => encodeRefs([{ ...ref('a'), root: fill(0xaa, 31) }]), 'store: field: ref root wants 32 bytes'],
    ['negative count', () => encodeRefs([{ ...ref('a'), count: -1n }]), 'store: field: ref count wants an unsigned integer'],
    ['short head', () => encodeRefs([{ ...ref('a'), head: fill(0xcc, 1) }]), 'store: field: ref head wants 32 bytes'],
    ['shadowed member', () => encodeRefs([{ ...ref('a'), unknown: [{ key: 'count', val: 9n }] }]), 'store: field: count is a defined ref member'],
    [
      'too wide',
      () => encodeRefs([{ ...ref('a'), unknown: 'bcdefg'.split('').map((k) => ({ key: k, val: '1' })) }]),
      `store: field: ref a has ${MaxRefMembers + 1} members`,
    ],
    ['too many', () => encodeRefs(Array.from({ length: MaxRefs + 1 }, (_, i) => ref(`s${i}`))), `store: field: ${MaxRefs + 1} refs`],
    ['not an array', () => decodeRefs(new CborMap()), 'store: field: refs'],
    ['absent', () => decodeRefs(undefined), 'store: field: refs'],
    ['not a map', () => decodeRefs(['x']), 'store: field: ref'],
    ['too narrow', () => decodeRefs([new CborMap([{ key: 'count', val: 1n }, { key: 'name', val: 'a' }])]), 'store: field: ref'],
    ['integer member', () => decodeRefs([new CborMap([...entry('a'), { key: 1n, val: 1n }])]), 'store: field: ref member key bigint'],
    ['decoded duplicate', () => decodeRefs([new CborMap(entry('a')), new CborMap(entry('a'))]), 'store: dup-ref: a'],
    ['decoded short root', () => decoded({ root: fill(0xaa, 31) }), 'store: field: ref root wants 32 bytes'],
    ['decoded negative count', () => decoded({ count: -1n }), 'store: field: ref count wants an unsigned integer'],
    ['decoded short head', () => decoded({ head: fill(0xcc, 1) }), 'store: field: ref head wants 32 bytes'],
    // Two faults in one entry: the root is checked before the count and the
    // head, so a reorder shows as a changed detail.
    ['decoded short root and negative count', () => decoded({ root: fill(0xaa, 31), count: -1n }), 'store: field: ref root wants 32 bytes'],
    ['decoded short head and short root', () => decoded({ root: fill(0xaa, 31), head: fill(0xcc, 1) }), 'store: field: ref root wants 32 bytes'],
  ]
  for (const [name, f, want] of cases) {
    const err = refused(f)
    assert.equal(err.message, want, name)
    assert.equal(err.message, `store: ${err.code}: ${err.detail}`, name)
  }
})

test('the width and count bounds hold at the edge on decode', () => {
  const wide = [...entry('a'), ...'bcdef'.split('').map((k) => ({ key: k, val: '1' as Value }))]
  assert.equal(wide.length, MaxRefMembers)
  assert.equal(decodeRefs([new CborMap(wide)])[0]!.unknown?.length, MaxRefMembers - 3)
  assert.equal(refused(() => decodeRefs([new CborMap([...wide, { key: 'g', val: '1' }])])).detail, 'ref')

  const many = Array.from({ length: MaxRefs }, (_, i) => new CborMap(entry(`s${i}`)))
  assert.equal(decodeRefs(many).length, MaxRefs)
  assert.equal(refused(() => decodeRefs([...many, new CborMap(entry('x'))])).detail, `${MaxRefs + 1} refs`)
  assert.equal(decodeRefs([new CborMap(entry('é'.repeat(MaxRefName / 2)))])[0]!.name.length, MaxRefName / 2, 'a name of exactly MaxRefName bytes')
})

function entry(name: string): Array<{ key: Value; val: Value }> {
  return [
    { key: 'name', val: name },
    { key: 'root', val: fill(0xaa) },
    { key: 'count', val: 1n },
  ]
}
