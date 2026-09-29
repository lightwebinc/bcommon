/**
 * How a record commits to a set of other records: the refs entry that names
 * a store and commits to its root. The twin of the Go package store's
 * EncodeRefs and DecodeRefs; the two hold the same bounds.
 *
 * The entry is the same for every application that stores records this way.
 * Where the entries sit in a record (which key, which kinds may carry them)
 * belongs to the application, so the codec works on the array value alone.
 * An application that words its own refusals maps StoreError's code and
 * detail onto its own error.
 */
import { CborMap, type Value } from './cbor.js'

/** The refs-entry members this version defines. */
const refMembers = new Set(['name', 'root', 'count', 'head'])

/**
 * MaxRefMembers bounds one refs entry, so an entry cannot carry unbounded
 * material under names this version does not define. Four are defined; the
 * rest is room for the format to grow without orphaning a reader.
 */
export const MaxRefMembers = 8

/**
 * MaxRefs bounds how many stores one record may commit to. A reader reads
 * every store a record links, so without it a record is an instruction to
 * make an unbounded number of requests. The Go codec holds the same bound;
 * a host that admitted more would hold a record no reader accepts.
 */
export const MaxRefs = 64

/** MaxRefName bounds a store name, in UTF-8 bytes as Go's len() counts. */
export const MaxRefName = 64

/**
 * `field` is an entry or a member of the wrong type, width or length;
 * `dup-ref` is two entries naming one store.
 */
export type StoreErrorCode = 'field' | 'dup-ref'

export class StoreError extends Error {
  constructor(
    readonly code: StoreErrorCode,
    readonly detail: string,
  ) {
    super(`store: ${code}: ${detail}`)
    this.name = 'StoreError'
  }
}

/**
 * A sub-store and its commitment: an RFC 6962 root over the commitments of
 * that store's sub-records, and how many there are.
 */
export interface Ref {
  name: string
  root: Uint8Array
  count: bigint
  /**
   * The commitment of the store's head member, in hash byte order. It is
   * what lets a reader find the store: a root cannot be inverted and the
   * host holds no root-to-member index. A one-member store's root is the
   * leaf hash of its member, so with count 1 a reader proves membership by
   * hashing head once. Absent, the store is committed to but not publicly
   * linked.
   */
  head?: Uint8Array
  /**
   * Members this version does not define, kept verbatim so a re-encode is
   * faithful and an older host does not refuse a newer publisher's record.
   * They are preserved, not ignored: a member a reader does not know may
   * change how membership is computed, so a READER refuses that store. A
   * host does not read stores at all, so it only has to carry them.
   */
  unknown?: Array<{ key: string; val: Value }>
}

const utf8 = new TextEncoder()

function fixed(v: Value | undefined, n: number, name: string): Uint8Array {
  if (!(v instanceof Uint8Array) || v.length !== n) throw new StoreError('field', `${name} wants ${n} bytes`)
  return v.slice()
}

function unsigned(v: Value | undefined, name: string): bigint {
  if (typeof v !== 'bigint' || v < 0n) throw new StoreError('field', `${name} wants an unsigned integer`)
  return v
}

/**
 * The refs entries as the array value a record carries, checking every
 * entry's shape. Throws StoreError.
 */
export function encodeRefs(refs: readonly Ref[]): Value[] {
  if (refs.length > MaxRefs) throw new StoreError('field', `${refs.length} refs`)
  const out: Value[] = []
  // A store name has to identify one root. The CBOR rules refuse a duplicate
  // map KEY, but refs is an array, so nothing upstream catches a second entry
  // with the same name, and two roots under one name makes "fetch the links
  // store" a question with two answers.
  const refNames = new Set<string>()
  for (const ref of refs) {
    if (ref.name === '' || utf8.encode(ref.name).length > MaxRefName) throw new StoreError('field', 'ref name')
    if (refNames.has(ref.name)) throw new StoreError('dup-ref', ref.name)
    refNames.add(ref.name)
    const entry: Array<{ key: string; val: Value }> = [
      { key: 'name', val: ref.name },
      { key: 'root', val: fixed(ref.root, 32, 'ref root') },
      { key: 'count', val: unsigned(ref.count, 'ref count') },
    ]
    if (ref.head !== undefined) entry.push({ key: 'head', val: fixed(ref.head, 32, 'ref head') })
    for (const p of ref.unknown ?? []) {
      if (refMembers.has(p.key)) throw new StoreError('field', `${p.key} is a defined ref member`)
      entry.push(p)
    }
    if (entry.length > MaxRefMembers) throw new StoreError('field', `ref ${ref.name} has ${entry.length} members`)
    out.push(new CborMap(entry))
  }
  return out
}

/**
 * Read a record's refs value, checking every entry's shape and preserving
 * the members this version does not define. Throws StoreError.
 */
export function decodeRefs(v: Value | undefined): Ref[] {
  if (!Array.isArray(v)) throw new StoreError('field', 'refs')
  if (v.length > MaxRefs) throw new StoreError('field', `${v.length} refs`)
  const refs: Ref[] = []
  const seenRef = new Set<string>()
  for (const e of v) {
    if (!(e instanceof CborMap) || e.entries.length < 3 || e.entries.length > MaxRefMembers) {
      throw new StoreError('field', 'ref')
    }
    const head = e.get('head')
    const name = e.get('name')
    if (typeof name !== 'string' || name === '' || utf8.encode(name).length > MaxRefName) {
      throw new StoreError('field', 'ref name')
    }
    if (seenRef.has(name)) throw new StoreError('dup-ref', name)
    seenRef.add(name)
    const ref: Ref = {
      name,
      root: fixed(e.get('root'), 32, 'ref root'),
      count: unsigned(e.get('count'), 'ref count'),
    }
    if (head !== undefined) ref.head = fixed(head, 32, 'ref head')
    // Members this version does not define are kept, not refused: refusing
    // makes one new store field cost every existing reader, and every
    // existing HOST, the whole record.
    const extra = e.entries.filter((p) => typeof p.key !== 'string' || !refMembers.has(p.key))
    for (const p of extra) if (typeof p.key !== 'string') throw new StoreError('field', `ref member key ${typeof p.key}`)
    if (extra.length > 0) ref.unknown = extra as Array<{ key: string; val: Value }>
    refs.push(ref)
  }
  return refs
}
