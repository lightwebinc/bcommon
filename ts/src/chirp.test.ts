/**
 * The byte-leaf roots and the CHIRP codec against bytetree-v1.json and
 * chirp-v1.json, the vectors the Go packages commit and chirp read: same
 * roots, paths, node bytes, closures and refusal codes.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createHash } from 'node:crypto'
import {
  ChirpError,
  ChunkSize,
  build,
  buildTree,
  chirpURL,
  decodeBranch,
  decodeRoot,
  encodeBranch,
  encodeRoot,
  identifier,
  parseChirpURL,
  parseIdentifier,
  validMediaType,
  verifyClosure,
  type Child,
} from './chirp.js'
import { CommitError, RootBuilder, SegmentWriter, hashLeaf, pathLength, proveBytes, rootOfBytes, segmentRoot, verifyAt, verifyBytes } from './commit.js'
import { fromHex, toHex } from './testing/index.js'

function vector<T>(name: string): T {
  return JSON.parse(readFileSync(new URL(`../../testdata/vectors/${name}`, import.meta.url), 'utf8')) as T
}

function stream(n: number): Uint8Array {
  const out = new Uint8Array(n + 32)
  let off = 0
  for (let j = 0; off < n; j++) {
    const pre = Buffer.from('bcommon vector content')
    const b = Buffer.alloc(pre.length + 4)
    pre.copy(b)
    b.writeUInt32BE(j, pre.length)
    out.set(createHash('sha256').update(b).digest(), off)
    off += 32
  }
  return out.subarray(0, n)
}

const code = (e: unknown): string => {
  if (e instanceof ChirpError || e instanceof CommitError) return e.code
  throw e
}

interface ByteTree {
  leaves: Array<{ hex: string }>
  trees: Array<{ size: number; rootHex: string; paths: Array<{ index: number; steps: Array<{ hashHex: string; left: boolean }> }> }>
  segmentSize: number
  objects: Array<{ size: number; segments: number; rootHex: string; samples: Array<{ index: number; pathLen: number; siblingsHex: string[] }> }>
}

test('byte-leaf roots, paths and compact paths match bytetree-v1.json', () => {
  const v = vector<ByteTree>('bytetree-v1.json')
  const leaves = v.leaves.map((l) => fromHex(l.hex))
  assert.equal(v.trees.length, leaves.length + 1)
  for (const tr of v.trees) {
    const d = leaves.slice(0, tr.size)
    const root = fromHex(tr.rootHex)
    assert.equal(toHex(rootOfBytes(d)), tr.rootHex, `n=${tr.size}`)
    const b = new RootBuilder()
    for (const l of d) b.addBytes(l)
    assert.equal(toHex(b.root()), tr.rootHex)
    for (const p of tr.paths) {
      const got = proveBytes(d, p.index)
      assert.deepEqual(got.map((s) => ({ hashHex: toHex(s.hash), left: s.left })), p.steps)
      assert.ok(verifyBytes(d[p.index]!, got, root))
      assert.ok(verifyAt(hashLeaf(d[p.index]!), p.index, tr.size, got.map((s) => s.hash), root))
    }
  }
  for (const o of v.objects) {
    const c = stream(o.size)
    assert.equal(toHex(segmentRoot(c, v.segmentSize)), o.rootHex, `size ${o.size}`)
    const w = new SegmentWriter(v.segmentSize)
    for (let off = 0; off < c.length; off += 1000) w.write(c.subarray(off, Math.min(off + 1000, c.length)))
    assert.equal(toHex(w.root()), o.rootHex)
    assert.equal(w.segments(), o.segments)
    const root = fromHex(o.rootHex)
    for (const s of o.samples) {
      const seg = c.subarray(s.index * v.segmentSize, Math.min((s.index + 1) * v.segmentSize, c.length))
      const sib = s.siblingsHex.map(fromHex)
      assert.equal(pathLength(s.index, o.segments), s.pathLen)
      assert.ok(verifyAt(hashLeaf(seg), s.index, o.segments, sib, root))
      if (sib.length > 0) {
        const bad = sib.map((x) => x.slice())
        bad[0]![0]! ^= 1
        assert.ok(!verifyAt(hashLeaf(seg), s.index, o.segments, bad, root))
        assert.ok(!verifyAt(hashLeaf(seg), s.index, o.segments, sib.slice(0, -1), root))
      }
      assert.ok(!verifyAt(hashLeaf(seg), s.index, o.segments, [...sib, root], root))
    }
  }
  assert.throws(() => pathLength(3, 3), (e) => code(e) === 'index')
  assert.equal(pathLength(2 ** 33, 2 ** 33 + 1), 1)
})

interface ChirpVector {
  golden: Array<{ name: string; contentHex: string; extensions: Array<{ type: number; valueHex: string }>; contentHashHex: string; rootHex: string; rootHashHex: string; identifier: string; url: string }>
  contents: Array<{ name: string; parts: Array<{ stream?: number; hex?: string }>; size: number; blobHashesHex: string[]; branchesHex: string[]; rootHex: string; rootHashHex: string; identifier: string; objects: number }>
  shapes: Array<{ leaves: number; branches: number; contentHashHex: string; rootChildren: Array<{ kind: number; length: number; hashHex: string }>; rootHashHex: string }>
  nodes: Array<{ name: string; as: string; hex: string; reason: string }>
  closures: Array<{ name: string; rootHashHex: string; objects: Array<{ hashHex: string; hex: string }>; maxReferences: number; maxLength: number; reason: string; canonical: boolean; blobs: number }>
  identifiers: Array<{ name: string; input: string; url: boolean; hashHex: string; reason: string }>
}

const cv = vector<ChirpVector>('chirp-v1.json')

test('BRC-167 golden roots and identifiers', () => {
  for (const g of cv.golden) {
    const b = build(fromHex(g.contentHex), g.extensions.map((e) => ({ type: BigInt(e.type), value: fromHex(e.valueHex) })))
    assert.equal(toHex(b.root), g.rootHex, g.name)
    assert.equal(toHex(b.rootHash), g.rootHashHex)
    assert.equal(identifier(b.rootHash), g.identifier)
    assert.equal(chirpURL(b.rootHash), g.url)
    const r = decodeRoot(b.root)
    assert.equal(toHex(r.contentHash), g.contentHashHex)
    assert.equal(toHex(encodeRoot(r)), g.rootHex)
  }
})

test('closures built from content, and verified', async () => {
  for (const c of cv.contents) {
    const parts = c.parts.map((p) => (p.hex !== undefined && p.hex !== '' ? fromHex(p.hex) : stream(p.stream ?? 0)))
    const content = new Uint8Array(parts.reduce((s, p) => s + p.length, 0))
    let off = 0
    for (const p of parts) (content.set(p, off), (off += p.length))
    assert.equal(content.length, c.size)
    const b = build(content)
    assert.equal(toHex(b.root), c.rootHex, c.name)
    assert.equal(toHex(b.rootHash), c.rootHashHex)
    assert.equal(identifier(b.rootHash), c.identifier)
    assert.deepEqual(b.branches.map(toHex), c.branchesHex)
    const sha = (x: Uint8Array): string => createHash('sha256').update(x).digest('hex')
    assert.deepEqual((b.blobs ?? []).map(sha), c.blobHashesHex)
    const objects = new Map<string, Uint8Array>([[toHex(b.rootHash), b.root]])
    for (const x of [...(b.blobs ?? []), ...b.branches]) objects.set(sha(x), x)
    const cl = await verifyClosure(b.rootHash, (h) => {
      const o = objects.get(toHex(h))
      if (o === undefined) throw new Error('none')
      return o
    })
    assert.ok(cl.canonical)
    assert.equal(cl.objects.length, c.objects)
  }
})

test('BRC-167 compact tree shapes', () => {
  for (const s of cv.shapes) {
    const refs: Child[] = []
    for (let i = 0; i < s.leaves; i++) {
      refs.push({ kind: 0, length: BigInt(ChunkSize), hash: Uint8Array.from(createHash('sha256').update(`BRC-167 canonical tree leaf:${i}`).digest()) })
    }
    const b = buildTree(refs, fromHex(s.contentHashHex))
    assert.equal(b.branches.length, s.branches)
    assert.equal(toHex(b.rootHash), s.rootHashHex, `${s.leaves} leaves`)
    const r = decodeRoot(b.root)
    assert.deepEqual(
      r.children.map((c) => ({ kind: c.kind, length: Number(c.length), hashHex: toHex(c.hash) })),
      s.rootChildren,
    )
  }
})

test('node decoding admits and refuses as the Go codec does', () => {
  for (const n of cv.nodes) {
    const b = fromHex(n.hex)
    let got = ''
    try {
      if (n.as === 'root') assert.equal(toHex(encodeRoot(decodeRoot(b))), n.hex)
      else assert.equal(toHex(encodeBranch(decodeBranch(b))), n.hex)
    } catch (e) {
      got = code(e)
    }
    assert.equal(got, n.reason, n.name)
  }
})

test('closure verification admits and refuses as the Go codec does', async () => {
  for (const c of cv.closures) {
    const objects = new Map(c.objects.map((o) => [o.hashHex, fromHex(o.hex)]))
    let got = ''
    try {
      const cl = await verifyClosure(
        fromHex(c.rootHashHex),
        async (h) => {
          const o = objects.get(toHex(h))
          if (o === undefined) throw new Error('none')
          return o
        },
        { maxReferences: c.maxReferences, maxLength: BigInt(c.maxLength) },
      )
      assert.equal(cl.canonical, c.canonical, c.name)
      assert.equal(cl.blobs.length, c.blobs, c.name)
    } catch (e) {
      got = code(e)
    }
    assert.equal(got, c.reason, c.name)
  }
})

test('identifiers and URLs', () => {
  for (const c of cv.identifiers) {
    let got = ''
    try {
      const h = c.url ? parseChirpURL(c.input) : parseIdentifier(c.input)
      assert.equal(toHex(h), c.hashHex, c.name)
    } catch (e) {
      got = code(e)
    }
    assert.equal(got, c.reason, c.name)
  }
})

test('the encoder refuses what the decoder refuses, and media types', () => {
  const hh = Uint8Array.from(createHash('sha256').update('hello').digest())
  const one: Child[] = [{ kind: 0, length: 5n, hash: hh }]
  const refuses = (f: () => unknown, want: string): void => assert.throws(f, (e) => code(e) === want)
  refuses(() => encodeRoot({ profile: 0, length: 5n, contentHash: hh, children: one, extensions: [] }), 'profile')
  refuses(() => encodeRoot({ profile: 1, length: 6n, contentHash: hh, children: one, extensions: [] }), 'length')
  refuses(() => encodeRoot({ profile: 1, length: 0n, contentHash: hh, children: [], extensions: [] }), 'empty')
  refuses(() => encodeRoot({ profile: 1, length: 5n, contentHash: hh, children: one, extensions: [{ type: 4n, value: new Uint8Array() }] }), 'extension')
  refuses(() => encodeBranch({ length: 0n, children: [], extensions: [] }), 'child-count')
  refuses(() => buildTree([...one, ...one], hh), 'canonical')
  for (const [s, ok] of [
    ['text/plain', true],
    ['application/vnd.example+json', true],
    ['Text/plain', false],
    ['text/', false],
    ['text/plain; charset=x', false],
    ['+text/plain', false],
  ] as const) {
    assert.equal(validMediaType(s), ok, s)
  }
  // Every truncation and single-byte change of a root is refused by a code
  // or re-encodes to itself.
  const root = build(new TextEncoder().encode('hello'), [{ type: 1n, value: new TextEncoder().encode('text/plain') }]).root
  const check = (b: Uint8Array): void => {
    try {
      assert.equal(toHex(encodeRoot(decodeRoot(b))), toHex(b))
    } catch (e) {
      code(e)
    }
  }
  for (let i = 0; i < root.length; i++) {
    check(root.subarray(0, i))
    for (const x of [0, 1, 0x7f, 0xfd, 0xff]) {
      const c = root.slice()
      c[i] = x
      check(c)
    }
  }
})
