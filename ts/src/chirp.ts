/**
 * CHIRP (BRC-167) version 1 and chunking profile 1: the twin of the Go
 * package chirp. Root and branch node codecs, profile 1's canonical
 * construction, closure verification and the UHRP object identifier. Every
 * refusal is a ChirpError whose code is the Go package's Reason for the
 * same bytes, in the same order of checks.
 *
 * Lengths and extension types are bigints, since a node carries 64-bit
 * values.
 */
import { Hash, Utils } from '@bsv/sdk'

export const ChunkSize = 4194304
export const Fanout = 256
export const MaxNode = 65536
export const MaxExtensionBytes = 16384
export const MaxDepth = 16
export const Profile1 = 1
export const MediaType = 1n
export const DefaultMaxReferences = 1 << 22

export const KindRoot = 0
export const KindBranch = 1
export const ChildBlob = 0
export const ChildBranch = 1

export type ChirpErrorCode =
  | 'node-size'
  | 'truncated'
  | 'magic'
  | 'version'
  | 'node-kind'
  | 'profile'
  | 'compact-size'
  | 'child-count'
  | 'child-kind'
  | 'length'
  | 'extension'
  | 'trailing'
  | 'empty'
  | 'missing'
  | 'hash'
  | 'depth'
  | 'cycle'
  | 'references'
  | 'too-large'
  | 'content-hash'
  | 'canonical'
  | 'identifier'

export class ChirpError extends Error {
  constructor(readonly code: ChirpErrorCode, detail = '') {
    super(`chirp: ${code}${detail === '' ? '' : `: ${detail}`}`)
    this.name = 'ChirpError'
  }
}

export interface Child {
  kind: number
  length: bigint
  hash: Uint8Array
}

export interface Extension {
  type: bigint
  value: Uint8Array
}

export interface Root {
  profile: number
  length: bigint
  contentHash: Uint8Array
  children: Child[]
  extensions: Extension[]
}

export interface Branch {
  length: bigint
  children: Child[]
  extensions: Extension[]
}

const magic = [0x43, 0x48, 0x49, 0x52, 0x50]
const u64max = (1n << 64n) - 1n

function sha256(...parts: Uint8Array[]): Uint8Array {
  const h = new Hash.SHA256()
  for (const p of parts) h.update(p)
  return Uint8Array.from(h.digest())
}

const emptyHash = sha256()

function equal(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}

const hex = (b: Uint8Array): string => Utils.toHex(Array.from(b))

// ---- encoding ----

class Writer {
  private parts: number[] = []
  bytes(b: Uint8Array | number[]): void {
    for (const x of b) this.parts.push(x)
  }
  u8(x: number): void {
    this.parts.push(x)
  }
  be(x: bigint, n: number): void {
    for (let i = n - 1; i >= 0; i--) this.parts.push(Number((x >> BigInt(8 * i)) & 0xffn))
  }
  le(x: bigint, n: number): void {
    for (let i = 0; i < n; i++) this.parts.push(Number((x >> BigInt(8 * i)) & 0xffn))
  }
  compact(v: bigint): void {
    if (v < 0xfdn) this.u8(Number(v))
    else if (v <= 0xffffn) (this.u8(0xfd), this.le(v, 2))
    else if (v <= 0xffffffffn) (this.u8(0xfe), this.le(v, 4))
    else (this.u8(0xff), this.le(v, 8))
  }
  done(): Uint8Array {
    return Uint8Array.from(this.parts)
  }
}

function header(w: Writer, kind: number): void {
  w.bytes(magic)
  w.u8(1)
  w.u8(0)
  w.u8(kind)
}

function tail(w: Writer, children: readonly Child[], ext: readonly Extension[]): void {
  w.compact(BigInt(children.length))
  for (const c of children) {
    w.u8(c.kind)
    w.be(c.length, 8)
    w.bytes(c.hash)
  }
  w.compact(BigInt(ext.length))
  for (const e of ext) {
    w.compact(e.type)
    w.compact(BigInt(e.value.length))
    w.bytes(e.value)
  }
}

function checkChildren(children: readonly Child[], length: bigint, branch: boolean): void {
  if (children.length > Fanout || (branch && children.length === 0)) throw new ChirpError('child-count', `${children.length} children`)
  let sum = 0n
  for (const [i, c] of children.entries()) {
    if (c.kind !== ChildBlob && c.kind !== ChildBranch) throw new ChirpError('child-kind', `child ${i} is kind ${c.kind}`)
    if (c.hash.length !== 32 || c.length < 0n || c.length > u64max) throw new ChirpError('length', `child ${i}`)
    sum += c.length
    if (sum > u64max) throw new ChirpError('length', 'child lengths overflow')
  }
  if (sum !== length) throw new ChirpError('length', `children sum to ${sum}, node says ${length}`)
}

function checkEmpty(r: Root): void {
  if (r.length === 0n && (r.children.length !== 0 || !equal(r.contentHash, emptyHash))) throw new ChirpError('empty')
}

function checkExtensions(ext: readonly Extension[], kind: number): void {
  let prev = 0n
  let total = 0
  for (const [i, e] of ext.entries()) {
    if (e.type === 0n || (i > 0 && e.type <= prev)) throw new ChirpError('extension', `type ${e.type} out of order or reserved`)
    prev = e.type
    total += e.value.length
    if (total > MaxExtensionBytes) throw new ChirpError('extension', `more than ${MaxExtensionBytes} value bytes`)
    if (e.type % 2n === 0n) throw new ChirpError('extension', `unknown critical type ${e.type}`)
    if (e.type === MediaType) {
      if (kind !== KindRoot) throw new ChirpError('extension', 'mediaType on a branch')
      if (!validMediaType(e.value)) throw new ChirpError('extension', 'mediaType')
    }
  }
}

const nameChars = new Set(Array.from('!#$&-^_.+', (c) => c.charCodeAt(0)))

/** Whether a mediaType value is a lower-case media-type essence BRC-167 admits, as Go's ValidMediaType. */
export function validMediaType(v: Uint8Array | string): boolean {
  const b = typeof v === 'string' ? new TextEncoder().encode(v) : v
  if (b.length < 3 || b.length > 127) return false
  let slash = -1
  for (let i = 0; i < b.length; i++) {
    const c = b[i]!
    if (c === 0x2f) {
      if (slash >= 0) return false
      slash = i
    } else if ((c >= 0x61 && c <= 0x7a) || (c >= 0x30 && c <= 0x39)) {
      // a letter or digit
    } else if (nameChars.has(c)) {
      if (i === 0 || i === slash + 1) return false
    } else {
      return false
    }
  }
  return slash > 0 && slash < b.length - 1
}

/** Writes a root node; refuses what decodeRoot refuses. */
export function encodeRoot(r: Root): Uint8Array {
  if (r.profile === 0) throw new ChirpError('profile')
  if (!Number.isInteger(r.profile) || r.profile < 0 || r.profile > 0xffff || r.contentHash.length !== 32) throw new ChirpError('length', 'root fields')
  checkChildren(r.children, r.length, false)
  checkEmpty(r)
  checkExtensions(r.extensions, KindRoot)
  const w = new Writer()
  header(w, KindRoot)
  w.be(BigInt(r.profile), 2)
  w.be(r.length, 8)
  w.bytes(r.contentHash)
  tail(w, r.children, r.extensions)
  const out = w.done()
  if (out.length > MaxNode) throw new ChirpError('node-size')
  return out
}

/** Writes a branch node; refuses what decodeBranch refuses. */
export function encodeBranch(b: Branch): Uint8Array {
  checkChildren(b.children, b.length, true)
  checkExtensions(b.extensions, KindBranch)
  const w = new Writer()
  header(w, KindBranch)
  w.be(b.length, 8)
  tail(w, b.children, b.extensions)
  const out = w.done()
  if (out.length > MaxNode) throw new ChirpError('node-size')
  return out
}

// ---- decoding ----

class Reader {
  off = 0
  constructor(readonly b: Uint8Array) {}
  take(n: number): Uint8Array {
    if (n < 0 || this.b.length - this.off < n) throw new ChirpError('truncated')
    const out = this.b.subarray(this.off, this.off + n)
    this.off += n
    return out
  }
  u8(): number {
    return this.take(1)[0]!
  }
  be(n: number): bigint {
    let v = 0n
    for (const x of this.take(n)) v = (v << 8n) | BigInt(x)
    return v
  }
  le(n: number): bigint {
    const b = this.take(n)
    let v = 0n
    for (let i = n - 1; i >= 0; i--) v = (v << 8n) | BigInt(b[i]!)
    return v
  }
  compact(): bigint {
    const p = this.u8()
    let v: bigint
    let floor: bigint
    if (p === 0xfd) (v = this.le(2)), (floor = 0xfdn)
    else if (p === 0xfe) (v = this.le(4)), (floor = 0x10000n)
    else if (p === 0xff) (v = this.le(8)), (floor = 0x100000000n)
    else return BigInt(p)
    if (v < floor) throw new ChirpError('compact-size')
    return v
  }
  remaining(): number {
    return this.b.length - this.off
  }
  header(): number {
    if (this.b.length > MaxNode) throw new ChirpError('node-size')
    const m = this.take(5)
    for (let i = 0; i < 5; i++) if (m[i] !== magic[i]) throw new ChirpError('magic')
    const major = this.u8()
    const minor = this.u8()
    if (major !== 1 || minor !== 0) throw new ChirpError('version', `${major}.${minor}`)
    const kind = this.u8()
    if (kind !== KindRoot && kind !== KindBranch) throw new ChirpError('node-kind', `${kind}`)
    return kind
  }
  tail(): { children: Child[]; extensions: Extension[] } {
    const n = this.compact()
    if (n > BigInt(Fanout)) throw new ChirpError('child-count', `${n} children`)
    const children: Child[] = []
    for (let i = 0; i < Number(n); i++) {
      const kind = this.u8()
      if (kind !== ChildBlob && kind !== ChildBranch) throw new ChirpError('child-kind', `child ${i} is kind ${kind}`)
      const length = this.be(8)
      const hash = this.take(32).slice()
      children.push({ kind, length, hash })
    }
    const ne = this.compact()
    if (ne > BigInt(Math.floor(this.remaining() / 2))) throw new ChirpError('truncated')
    const extensions: Extension[] = []
    for (let i = 0n; i < ne; i++) {
      const type = this.compact()
      const l = this.compact()
      if (l > BigInt(this.remaining())) throw new ChirpError('truncated')
      extensions.push({ type, value: this.take(Number(l)).slice() })
    }
    if (this.off !== this.b.length) throw new ChirpError('trailing')
    return { children, extensions }
  }
}

/** The kind of a node from its prefix, after the size, magic and version checks. */
export function nodeKind(b: Uint8Array): number {
  return new Reader(b).header()
}

/** Reads a root node, with the Go codec's checks in its order. */
export function decodeRoot(b: Uint8Array): Root {
  const r = new Reader(b)
  if (r.header() !== KindRoot) throw new ChirpError('node-kind', 'a branch where a root is wanted')
  const profile = Number(r.be(2))
  if (profile === 0) throw new ChirpError('profile')
  const length = r.be(8)
  const contentHash = r.take(32).slice()
  const { children, extensions } = r.tail()
  const out: Root = { profile, length, contentHash, children, extensions }
  checkChildren(children, length, false)
  checkEmpty(out)
  checkExtensions(extensions, KindRoot)
  return out
}

/** Reads a branch node, with the Go codec's checks in its order. */
export function decodeBranch(b: Uint8Array): Branch {
  const r = new Reader(b)
  if (r.header() !== KindBranch) throw new ChirpError('node-kind', 'a root where a branch is wanted')
  const length = r.be(8)
  const { children, extensions } = r.tail()
  checkChildren(children, length, true)
  checkExtensions(extensions, KindBranch)
  return { length, children, extensions }
}

// ---- construction ----

export interface Built {
  root: Uint8Array
  rootHash: Uint8Array
  /** Branch nodes, lowest level first, left to right. */
  branches: Uint8Array[]
  /** The blobs, when built from content. */
  blobs?: Uint8Array[]
}

/** Profile 1's canonical tree over blob references, as Go's BuildTree. */
export function buildTree(blobs: readonly Child[], contentHash: Uint8Array, extensions: readonly Extension[] = []): Built {
  let length = 0n
  for (const [i, c] of blobs.entries()) {
    if (c.kind !== ChildBlob) throw new ChirpError('canonical', `reference ${i} is not a blob`)
    const last = i === blobs.length - 1
    const size = BigInt(ChunkSize)
    if ((!last && c.length !== size) || (last && (c.length === 0n || c.length > size))) throw new ChirpError('canonical', `blob ${i} is ${c.length} bytes`)
    length += c.length
  }
  if (length > u64max) throw new ChirpError('length', 'content longer than 2^64 bytes')
  const branches: Uint8Array[] = []
  let level: Child[] = [...blobs]
  while (level.length > Fanout) {
    const next: Child[] = []
    for (let i = 0; i < level.length; i += Fanout) {
      const group = level.slice(i, i + Fanout)
      const sum = group.reduce((s, c) => s + c.length, 0n)
      const b = encodeBranch({ length: sum, children: group, extensions: [] })
      branches.push(b)
      next.push({ kind: ChildBranch, length: sum, hash: sha256(b) })
    }
    level = next
  }
  const root = encodeRoot({ profile: Profile1, length, contentHash, children: level, extensions: [...extensions] })
  return { root, rootHash: sha256(root), branches }
}

/** Profile 1's closure of content held in memory. */
export function build(content: Uint8Array, extensions: readonly Extension[] = []): Built {
  const refs: Child[] = []
  const blobs: Uint8Array[] = []
  for (let off = 0; off < content.length; off += ChunkSize) {
    const b = content.subarray(off, Math.min(off + ChunkSize, content.length))
    blobs.push(b)
    refs.push({ kind: ChildBlob, length: BigInt(b.length), hash: sha256(b) })
  }
  const out = buildTree(refs, sha256(content), extensions)
  out.blobs = blobs
  return out
}

// ---- closures ----

export type Fetch = (hash: Uint8Array) => Promise<Uint8Array> | Uint8Array

export interface Limits {
  maxReferences?: number
  maxLength?: bigint
}

export interface Closure {
  root: Root
  rootHash: Uint8Array
  blobs: Child[]
  /** Distinct objects, root first, in depth-first child order. */
  objects: Uint8Array[]
  canonical: boolean
}

async function get(fetch: Fetch, h: Uint8Array): Promise<Uint8Array> {
  try {
    return await fetch(h)
  } catch (e) {
    throw new ChirpError('missing', `${hex(h)}: ${e instanceof Error ? e.message : String(e)}`)
  }
}

/** Fetches and checks a closure, in the order of Go's Verify. */
export async function verifyClosure(rootHash: Uint8Array, fetch: Fetch, limits: Limits = {}): Promise<Closure> {
  const maxRefs = limits.maxReferences !== undefined && limits.maxReferences > 0 ? limits.maxReferences : DefaultMaxReferences
  const rb = await get(fetch, rootHash)
  if (!equal(sha256(rb), rootHash)) throw new ChirpError('hash', 'the root')
  const root = decodeRoot(rb)
  if (limits.maxLength !== undefined && limits.maxLength > 0n && root.length > limits.maxLength) throw new ChirpError('too-large', `${root.length} bytes`)
  const seen = new Set<string>([hex(rootHash)])
  const branches = new Map<string, Branch>()
  const ancestry = new Set<string>([hex(rootHash)])
  const content = new Hash.SHA256()
  const out: Closure = { root, rootHash, blobs: [], objects: [rootHash], canonical: false }
  let refs = 0
  const note = (h: Uint8Array): void => {
    const k = hex(h)
    if (!seen.has(k)) {
      seen.add(k)
      out.objects.push(h)
    }
  }
  const walk = async (list: readonly Child[], depth: number): Promise<void> => {
    for (const c of list) {
      refs++
      if (refs > maxRefs) throw new ChirpError('references')
      if (depth + 1 > MaxDepth) throw new ChirpError('depth')
      const k = hex(c.hash)
      if (c.kind === ChildBranch) {
        if (ancestry.has(k)) throw new ChirpError('cycle')
        let br = branches.get(k)
        if (br === undefined) {
          const b = await get(fetch, c.hash)
          if (!equal(sha256(b), c.hash)) throw new ChirpError('hash', `branch ${k}`)
          br = decodeBranch(b)
          branches.set(k, br)
        }
        if (br.length !== c.length) throw new ChirpError('length', `branch ${k} covers ${br.length} bytes, its reference ${c.length}`)
        note(c.hash)
        ancestry.add(k)
        await walk(br.children, depth + 1)
        ancestry.delete(k)
        continue
      }
      const b = await get(fetch, c.hash)
      if (BigInt(b.length) !== c.length) throw new ChirpError('length', `blob ${k} is ${b.length} bytes, its reference ${c.length}`)
      if (!equal(sha256(b), c.hash)) throw new ChirpError('hash', `blob ${k}`)
      content.update(b)
      out.blobs.push(c)
      note(c.hash)
    }
  }
  await walk(root.children, 1)
  if (!equal(Uint8Array.from(content.digest()), root.contentHash)) throw new ChirpError('content-hash')
  if (root.profile === Profile1) {
    let built: Built
    try {
      built = buildTree(out.blobs, root.contentHash, root.extensions)
    } catch (e) {
      throw new ChirpError('canonical', e instanceof Error ? e.message : String(e))
    }
    if (!equal(built.rootHash, rootHash)) throw new ChirpError('canonical', 'another grouping of the same blobs')
    out.canonical = true
  }
  return out
}

// ---- identifiers ----

const uhrpPrefix = [0xce, 0x00]

/** The UHRP object identifier of a hash: Base58Check of ce00 and the hash. */
export function identifier(hash: Uint8Array): string {
  return Utils.toBase58Check(Array.from(hash), uhrpPrefix)
}

/** The hash an object identifier names. Throws ChirpError `identifier`. */
export function parseIdentifier(s: string): Uint8Array {
  let data: number[]
  let prefix: number[]
  try {
    const r = Utils.fromBase58Check(s, undefined, 2)
    data = r.data as number[]
    prefix = r.prefix as number[]
  } catch {
    throw new ChirpError('identifier')
  }
  if (prefix.length !== 2 || prefix[0] !== 0xce || prefix[1] !== 0x00 || data.length !== 32) throw new ChirpError('identifier')
  const h = Uint8Array.from(data)
  if (identifier(h) !== s) throw new ChirpError('identifier')
  return h
}

/** chirp://<identifier>. */
export function chirpURL(root: Uint8Array): string {
  return 'chirp://' + identifier(root)
}

/** The root a CHIRP URI names: chirp://<id> or chirp:<id>, scheme in any case. */
export function parseChirpURL(s: string): Uint8Array {
  if (s.length < 6 || s.slice(0, 6).toLowerCase() !== 'chirp:') throw new ChirpError('identifier')
  let rest = s.slice(6)
  if (rest.startsWith('//')) rest = rest.slice(2)
  return parseIdentifier(rest)
}
