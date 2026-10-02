/**
 * A lock-before PushDrop read leniently and rebuilt canonically, over raw
 * script bytes: the twin of the Go pushdrop.FirstPush, Fields and Script.
 *
 * A host holds a script to one encoding by rebuilding it from the fields it
 * read and comparing bytes, so it need not trust a decoder to refuse every
 * other encoding. The reader is this package's own rather than the SDK's,
 * so that the two languages read every byte string the same way.
 */

const OP_0 = 0x00
const OP_PUSHDATA1 = 0x4c
const OP_PUSHDATA2 = 0x4d
const OP_PUSHDATA4 = 0x4e
const OP_1NEGATE = 0x4f
const OP_DROP = 0x75
const OP_2DROP = 0x6d
const OP_CHECKSIG = 0xac

function concat(parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0))
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.length
  }
  return out
}

/**
 * The push that follows a lock-before PushDrop's key and OP_CHECKSIG, and
 * nothing else of the script: the script starts with 0x21, 33 bytes and
 * 0xac, then one push, an opcode 0x01 to 0x4b or OP_PUSHDATA1, 2 or 4 with
 * its little-endian length, every declared byte present. It returns a view
 * of s.
 *
 * It is the first look a classifier takes at an output: which record or tag
 * the first field claims. A script that claims something is then held to
 * the exact script rebuilt from its fields (pushDropScript).
 */
export function firstPush(s: Uint8Array): Uint8Array | undefined {
  if (s.length < 36 || s[0] !== 0x21 || s[34] !== OP_CHECKSIG) return undefined
  const p = s.subarray(35)
  const op = p[0]!
  let n: number
  let off: number
  if (op >= 0x01 && op <= 0x4b) {
    n = op
    off = 1
  } else if (op === OP_PUSHDATA1 && p.length >= 2) {
    n = p[1]!
    off = 2
  } else if (op === OP_PUSHDATA2 && p.length >= 3) {
    n = p[1]! | (p[2]! << 8)
    off = 3
  } else if (op === OP_PUSHDATA4 && p.length >= 5) {
    n = (p[1]! | (p[2]! << 8) | (p[3]! << 16)) + p[4]! * 0x1000000
    if (n > p.length) return undefined
    off = 5
  } else {
    return undefined
  }
  if (p.length < off + n) return undefined
  return p.subarray(off, off + n)
}

/** One parsed script element; data is set for a data push. */
interface Op {
  code: number
  data?: Uint8Array
}

/** Splits s into opcodes; a push running past the end refuses the whole script. */
function parseScript(s: Uint8Array): Op[] | undefined {
  const out: Op[] = []
  let i = 0
  while (i < s.length) {
    const c = s[i++]!
    let n: number
    if (c === OP_0) {
      out.push({ code: c, data: new Uint8Array(0) })
      continue
    } else if (c >= 1 && c <= 75) {
      n = c
    } else if (c === OP_PUSHDATA1) {
      if (i + 1 > s.length) return undefined
      n = s[i]!
      i += 1
    } else if (c === OP_PUSHDATA2) {
      if (i + 2 > s.length) return undefined
      n = s[i]! | (s[i + 1]! << 8)
      i += 2
    } else if (c === OP_PUSHDATA4) {
      if (i + 4 > s.length) return undefined
      n = (s[i]! | (s[i + 1]! << 8) | (s[i + 2]! << 16)) + s[i + 3]! * 0x1000000
      i += 4
    } else {
      out.push({ code: c })
      continue
    }
    if (n > s.length - i) return undefined
    out.push({ code: c, data: s.subarray(i, i + n) })
    i += n
  }
  return out
}

/**
 * A lock-before PushDrop read leniently: a 33-byte push, OP_CHECKSIG, then
 * every data push up to the first opcode that is not one, the field
 * signature included when the lock carries one. Every field is a view of s.
 *
 * It decides nothing about the script: whether s is the canonical one is
 * decided by rebuilding it from these fields (pushDropScript) and comparing
 * bytes.
 *
 * A data push here is OP_0, a direct push or OP_PUSHDATA1, 2 or 4. OP_1 to
 * OP_16 and OP_1NEGATE are not, although the canonical lock writes a
 * one-byte field of 1 to 16 or 0x81 with them: such a field ends the
 * reading, the rebuild then differs, and a host that compares refuses the
 * script. A tag or a record is never one of those bytes alone. OP_0 reads
 * as an empty field, and rebuilds as OP_0.
 */
export function pushFields(s: Uint8Array): Uint8Array[] | undefined {
  const ops = parseScript(s)
  if (ops === undefined || ops.length < 3 || ops[0]!.data?.length !== 33 || ops[1]!.code !== OP_CHECKSIG) return undefined
  const fields: Uint8Array[] = []
  for (const o of ops.slice(2)) {
    if (o.data === undefined) break
    fields.push(o.data)
  }
  return fields.length > 0 ? fields : undefined
}

/** The minimal push of d, as the SDKs write it. */
export function minimalPushBytes(d: Uint8Array): Uint8Array {
  const n = d.length
  if (n === 0 || (n === 1 && d[0] === 0)) return Uint8Array.of(OP_0)
  if (n === 1 && d[0]! >= 1 && d[0]! <= 16) return Uint8Array.of(0x50 + d[0]!)
  if (n === 1 && d[0] === 0x81) return Uint8Array.of(OP_1NEGATE)
  let head: number[]
  if (n <= 75) head = [n]
  else if (n <= 255) head = [OP_PUSHDATA1, n]
  else if (n <= 65535) head = [OP_PUSHDATA2, n & 0xff, n >> 8]
  else head = [OP_PUSHDATA4, n & 0xff, (n >> 8) & 0xff, (n >> 16) & 0xff, (n >>> 24) & 0xff]
  return concat([Uint8Array.from(head), d])
}

/**
 * The one canonical lock-before PushDrop for a 33-byte key, fields and an
 * optional field signature, exactly as the producer's template writes it:
 * the key, OP_CHECKSIG, each field and then the signature as one minimal
 * push, and the fewest OP_2DROP then OP_DROP that clear them. A host
 * compares a script it checks with this, byte for byte.
 */
export function pushDropScript(key: Uint8Array, fields: Uint8Array[], sig?: Uint8Array): Uint8Array {
  const all = sig === undefined ? fields : [...fields, sig]
  const parts: Uint8Array[] = [Uint8Array.of(33), key, Uint8Array.of(OP_CHECKSIG), ...all.map(minimalPushBytes)]
  for (let n = all.length; n > 0; n -= 2) parts.push(Uint8Array.of(n === 1 ? OP_DROP : OP_2DROP))
  return concat(parts)
}
