/**
 * The TypeScript twins against the vectors the Go packages' tests read,
 * byte for byte, so that the two languages are held to one set of bytes.
 * The vectors come from the independent generator in tools/vectors (see
 * docs/vectors.md) and are read from the repository's testdata/vectors,
 * never copied: a vector regenerated for Go is the vector checked here.
 *
 * Every family a twin covers is read: CBOR, the manifest body (a CBOR value
 * the codec must reproduce), the refs entries, and the transactions' derived
 * keys, locks, field signatures, funding outputs and carriers, the
 * carrier unlocking-script cases, the public-key cases and the PushDrop
 * encoding cases. RFC 6962 roots and the transaction
 * builders have no twin here.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import {
  LockingScript,
  PrivateKey,
  ProtoWallet,
  PublicKey,
  PushDrop,
  Script,
  Transaction,
  type WalletInterface,
  type WalletProtocol,
} from '@bsv/sdk'
import { LockTime, SigHashType, commitment, decodeCarrier, inspectScript, mineableRefusal, unlockingRefusal, type PayloadCodec } from './carrier.js'
import { CborMap, decodeValue, encode, type Value } from './cbor.js'
import { readerLockingKey } from './derive.js'
import { verifyFieldSignature } from './fieldsig.js'
import { decodeFunding } from './funding.js'
import { strictPublicKey } from './pubkey.js'
import { decodeStrictPushDrop } from './pushdrop.js'
import { decodeRefs, encodeRefs, type Ref } from './store.js'
import { fromHex, toHex } from './testing/index.js'

// dist/vectors.test.js -> ts/ -> the repository root.
function vector<T>(name: string): T {
  return JSON.parse(readFileSync(new URL(`../../testdata/vectors/${name}`, import.meta.url), 'utf8')) as T
}

const hex = (b: Uint8Array | number[]): string => toHex(b)

/** One item of cbor-v1.json's value, described by its major type. */
interface Node {
  type: string
  value?: string
  items?: Node[]
  pairs?: Array<{ key: Node; value: Node }>
}

/** The node rebuilt in this codec's own types, as the Go test rebuilds it in its. */
function build(n: Node): Value {
  switch (n.type) {
    case 'uint':
      return BigInt(n.value ?? '')
    case 'neg': {
      const v = BigInt(n.value ?? '')
      assert.ok(v < 0n, `neg ${n.value}`)
      return v
    }
    case 'bytes':
      return fromHex(n.value ?? '')
    case 'text':
      return n.value ?? ''
    case 'array':
      return (n.items ?? []).map(build)
    case 'map':
      return new CborMap((n.pairs ?? []).map((p) => ({ key: build(p.key), val: build(p.value) })))
    case 'true':
      return true
    case 'false':
      return false
    case 'null':
      return null
  }
  assert.fail(`unknown node type ${n.type}`)
}

test('cbor-v1: the value encodes to the independent encoder\'s bytes, and they decode and re-encode unchanged', () => {
  const v = vector<{ value: Node; encodingHex: string }>('cbor-v1.json')
  // A value described under a key this test does not read would pass by
  // encoding nothing.
  assert.ok((v.value.pairs?.length ?? 0) > 0, 'the vector value has no pairs')
  assert.equal(hex(encode(build(v.value))), v.encodingHex)
  const back = decodeValue(fromHex(v.encodingHex))
  assert.equal(hex(encode(back)), v.encodingHex)
})

interface ManifestVector {
  members: Array<{ cHex: string; name: string; size: number; type: string }>
  bodyHex: string
}

test('manifest-v1: the body is rebuilt from its members byte for byte, and decodes to them', () => {
  const v = vector<ManifestVector>('manifest-v1.json')
  assert.ok(v.members.length > 0, 'the vector has no members')
  const member = (m: ManifestVector['members'][number]): CborMap =>
    // Listed out of canonical order on purpose: the encoder sorts.
    new CborMap([
      { key: 'type', val: m.type },
      { key: 'size', val: BigInt(m.size) },
      { key: 'name', val: m.name },
      { key: 'c', val: fromHex(m.cHex) },
    ])
  const body = new CborMap([{ key: 'members', val: v.members.map(member) }])
  assert.equal(hex(encode(body)), v.bodyHex)

  const back = decodeValue(fromHex(v.bodyHex))
  assert.ok(back instanceof CborMap)
  const members = back.get('members')
  assert.ok(Array.isArray(members))
  assert.equal(members.length, v.members.length)
  members.forEach((got, i) => {
    const want = v.members[i]!
    assert.ok(got instanceof CborMap, `member ${i}`)
    const c = got.get('c')
    assert.ok(c instanceof Uint8Array, `member ${i} c`)
    assert.equal(hex(c), want.cHex, `member ${i} c`)
    assert.equal(got.get('name'), want.name, `member ${i} name`)
    assert.equal(got.get('size'), BigInt(want.size), `member ${i} size`)
    assert.equal(got.get('type'), want.type, `member ${i} type`)
  })
  assert.equal(hex(encode(back)), v.bodyHex)
})

interface RefsVector {
  entries: Array<{ name: string; rootHex: string; count: number; headHex?: string; unknown?: Array<{ key: string; valueHex: string }> }>
  encodingHex: string
}

test('refs-v1: the entries encode to the vector, and it decodes to them with head and unknown members kept', () => {
  const v = vector<RefsVector>('refs-v1.json')
  assert.ok(v.entries.some((e) => e.headHex !== undefined), 'no entry has a head')
  assert.ok(v.entries.some((e) => (e.unknown?.length ?? 0) > 0), 'no entry has an unknown member')
  // An unknown member's value is recorded as its own CBOR encoding.
  const refs: Ref[] = v.entries.map((e) => ({
    name: e.name,
    root: fromHex(e.rootHex),
    count: BigInt(e.count),
    ...(e.headHex === undefined ? {} : { head: fromHex(e.headHex) }),
    ...(e.unknown === undefined ? {} : { unknown: e.unknown.map((u) => ({ key: u.key, val: decodeValue(fromHex(u.valueHex)) })) }),
  }))
  assert.equal(hex(encode(encodeRefs(refs))), v.encodingHex)

  const back = decodeRefs(decodeValue(fromHex(v.encodingHex)))
  assert.equal(back.length, v.entries.length)
  back.forEach((got, i) => {
    const want = v.entries[i]!
    assert.equal(got.name, want.name, `entry ${i} name`)
    assert.equal(hex(got.root), want.rootHex, `entry ${i} root`)
    assert.equal(got.count, BigInt(want.count), `entry ${i} count`)
    assert.equal(got.head === undefined ? undefined : hex(got.head), want.headHex, `entry ${i} head`)
    assert.deepEqual(
      (got.unknown ?? []).map((u) => ({ key: u.key, valueHex: hex(encode(u.val)) })).sort((a, b) => a.key.localeCompare(b.key)),
      [...(want.unknown ?? [])].sort((a, b) => a.key.localeCompare(b.key)),
      `entry ${i} unknown members`,
    )
  })
  assert.equal(hex(encode(encodeRefs(back))), v.encodingHex, 'a re-encode is faithful')
})

interface TxVector {
  privateKeyHex: string
  identityKeyHex: string
  securityLevel: number
  protocol: string
  objectKeyId: string
  stateKeyId: string
  objectLockingKeyHex: string
  stateLockingKeyHex: string
  fundingTagHex: string
  stateTagHex: string
  coin: { txid: string; txHex: string; bumpHex: string }
  fundingLockHex: string
  fundingTree: { count: number; txid: string; txHex: string; keptBeefHex: string }
  carriers: Array<{ vout: number; payloadHex: string; lockHex: string; commitmentHex: string; txid: string; txHex: string }>
  create: { carrier: number; lockHex: string; txid: string; txHex: string }
  update: { carrier: number; lockHex: string; txid: string; txHex: string }
  payment: { txid: string; txHex: string }
  sweep: { vouts: number[]; txid: string; txHex: string }
  sweepWithFee: { vouts: number[]; txid: string; txHex: string }
}

interface Chain {
  v: TxVector
  protocol: WalletProtocol
  fundingTag: number[]
  stateTag: number[]
  wallet: WalletInterface
}

function chain(): Chain {
  const v = vector<TxVector>('transactions-v1.json')
  // A family that is empty would pass every loop over it by running none of it.
  assert.ok(v.carriers.length > 0 && v.fundingTree.count > 0, 'a transaction family is missing or renamed')
  const priv = PrivateKey.fromHex(v.privateKeyHex)
  assert.equal(priv.toPublicKey().toString(), v.identityKeyHex, 'the identity is not the vector key\'s')
  return {
    v,
    protocol: [v.securityLevel as WalletProtocol[0], v.protocol],
    fundingTag: Array.from(fromHex(v.fundingTagHex)),
    stateTag: Array.from(fromHex(v.stateTagHex)),
    // ProtoWallet has the two calls PushDrop.lock makes, not the action surface.
    wallet: new ProtoWallet(priv) as unknown as WalletInterface,
  }
}

test('transactions-v1: every transaction parses and serialises to its own bytes and txid', () => {
  const { v } = chain()
  const all: Array<[string, { txHex: string; txid?: string }]> = [
    ['coin', v.coin],
    ['funding tree', v.fundingTree],
    ...v.carriers.map((c, i): [string, typeof c] => [`carrier ${i}`, c]),
    ['create', v.create],
    ['update', v.update],
    ['payment', v.payment],
    ['sweep', v.sweep],
    ['sweepWithFee', v.sweepWithFee],
  ]
  for (const [name, t] of all) {
    const tx = Transaction.fromHex(t.txHex)
    assert.equal(tx.toHex(), t.txHex, name)
    if (t.txid !== undefined) assert.equal(tx.id('hex'), t.txid, name)
  }
})

test('transactions-v1: the reader derives both locking keys the producer locked to', () => {
  const { v, protocol } = chain()
  assert.equal(readerLockingKey(protocol, v.objectKeyId, v.identityKeyHex).toString(), v.objectLockingKeyHex)
  assert.equal(readerLockingKey(protocol, v.stateKeyId, v.identityKeyHex).toString(), v.stateLockingKeyHex)
})

test('transactions-v1: the funding lock and every tree output decode as funding to the object key, under the funding tag only', async () => {
  const { v, protocol, fundingTag, stateTag, wallet } = chain()
  const lock = Script.fromHex(v.fundingLockHex)
  assert.equal(decodeFunding(lock, fundingTag)?.toString(), v.objectLockingKeyHex)
  assert.equal(decodeFunding(lock, stateTag), undefined, 'the state tag')
  // The SDK's own PushDrop writes the same script from the same inputs.
  const written = await new PushDrop(wallet).lock([fundingTag], protocol, v.objectKeyId, 'anyone', true, false, 'before')
  assert.equal(written.toHex(), v.fundingLockHex)

  const tree = Transaction.fromHex(v.fundingTree.txHex)
  assert.ok(tree.outputs.length >= v.fundingTree.count)
  for (let i = 0; i < v.fundingTree.count; i++) {
    assert.equal(decodeFunding(tree.outputs[i]!.lockingScript, fundingTag)?.toString(), v.objectLockingKeyHex, `tree output ${i}`)
  }
  // The tree's change is the coin's key, not a funding output.
  for (let i = v.fundingTree.count; i < tree.outputs.length; i++) {
    assert.equal(decodeFunding(tree.outputs[i]!.lockingScript, fundingTag), undefined, `tree output ${i}`)
  }
  // Both sweeps leave a funding-shaped tombstone.
  for (const [name, s] of [['sweep', v.sweep], ['sweepWithFee', v.sweepWithFee]] as const) {
    const tx = Transaction.fromHex(s.txHex)
    assert.equal(decodeFunding(tx.outputs[0]!.lockingScript, fundingTag)?.toString(), v.objectLockingKeyHex, name)
  }
})

test('transactions-v1: each carrier decodes to its payload, key and commitment, and its lock is what the SDK writes', async () => {
  const { v, protocol, wallet } = chain()
  const payloads = v.carriers.map((c) => c.payloadHex)
  // The vector's payloads belong to no application, so the codec takes
  // exactly those bytes and nothing else, as the Go test's classifier does.
  const codec: PayloadCodec<string> = {
    inspect: (b) => {
      const h = hex(b)
      return payloads.includes(h) ? { kind: 'payload', payload: h } : { kind: 'not-payload' }
    },
    validate: () => undefined,
  }
  const lockingKeyFor = (): ReturnType<typeof readerLockingKey> => readerLockingKey(protocol, v.objectKeyId, v.identityKeyHex)
  for (const [i, want] of v.carriers.entries()) {
    const tx = Transaction.fromHex(want.txHex)
    const c = decodeCarrier(tx, codec, lockingKeyFor)
    assert.ok(typeof c !== 'string', `carrier ${i} refused: ${String(c)}`)
    assert.equal(c.outputIndex, 0, `carrier ${i}`)
    assert.equal(hex(c.payloadBytes), want.payloadHex, `carrier ${i} payload`)
    assert.equal(c.lockingKey.toString(), v.objectLockingKeyHex, `carrier ${i} key`)
    assert.equal(hex(c.c), want.commitmentHex, `carrier ${i} commitment`)
    assert.equal(hex(commitment(tx)), want.commitmentHex, `carrier ${i} commitment`)
    assert.equal(tx.lockTime, LockTime, `carrier ${i} locktime`)
    assert.equal(tx.outputs[0]!.lockingScript.toHex(), want.lockHex, `carrier ${i} lock`)
    const written = await new PushDrop(wallet).lock([Array.from(fromHex(want.payloadHex))], protocol, v.objectKeyId, 'anyone', true, true, 'before')
    assert.equal(written.toHex(), want.lockHex, `carrier ${i}: the SDK's PushDrop writes another lock`)
  }
  // Every transaction the vector mines is refused as a carrier.
  for (const [name, t] of [['funding tree', v.fundingTree], ['create', v.create], ['sweep', v.sweep]] as const) {
    assert.equal(mineableRefusal(Transaction.fromHex(t.txHex)), 'mineable', name)
  }
})

interface UnlockingVector {
  carrier: number
  sigHashType: number
  cases: Array<{ name: string; accept: boolean; scriptValid: boolean; unlockingHex: string; txid: string; txHex: string }>
}

// Carrier 0 with its unlocking script rewritten, as the Go test reads it:
// only the canonical case is taken, by unlockingRefusal and by
// decodeCarrier alike. The interpreter's verdict (scriptValid) is the Go
// side's; the cases it accepts are why the check exists.
test('unlocking-v1: only the canonical unlocking script is taken', () => {
  const { v, protocol } = chain()
  const u = vector<UnlockingVector>('unlocking-v1.json')
  assert.equal(u.sigHashType, SigHashType)
  const base = v.carriers[u.carrier]
  assert.ok(base !== undefined, `no carrier ${u.carrier}`)
  const codec: PayloadCodec<string> = {
    inspect: (b) => (hex(b) === base.payloadHex ? { kind: 'payload', payload: base.payloadHex } : { kind: 'not-payload' }),
    validate: () => undefined,
  }
  const lockingKeyFor = (): ReturnType<typeof readerLockingKey> => readerLockingKey(protocol, v.objectKeyId, v.identityKeyHex)
  let accepted = 0
  let malleable = 0
  for (const c of u.cases) {
    const tx = Transaction.fromHex(c.txHex)
    assert.equal(tx.toHex(), c.txHex, `${c.name}: re-serialises to its own bytes`)
    assert.equal(tx.id('hex'), c.txid, `${c.name}: txid`)
    assert.equal(tx.inputs[0]!.unlockingScript!.toHex(), c.unlockingHex, `${c.name}: unlocking script`)
    assert.equal(c.accept, c.txHex === base.txHex, `${c.name}: only carrier ${u.carrier}'s own bytes are accepted`)
    assert.equal(unlockingRefusal(tx), c.accept ? undefined : 'non-canonical-unlocking', c.name)
    const d = decodeCarrier(tx, codec, lockingKeyFor)
    if (c.accept) assert.ok(typeof d !== 'string', `${c.name} refused: ${String(d)}`)
    else assert.equal(d, 'non-canonical-unlocking', c.name)
    if (c.accept) accepted++
    else if (c.scriptValid) malleable++
  }
  assert.ok(u.cases.length >= 21 && accepted === 1 && malleable > 0, `${u.cases.length} cases, ${accepted} accepted, ${malleable} malleable`)
})

test('transactions-v1: each state token\'s field signature verifies over its tag and commitment, and its lock is what the SDK writes', async () => {
  const { v, protocol, stateTag, wallet } = chain()
  for (const [name, t] of [['create', v.create], ['update', v.update]] as const) {
    const commitmentBytes = Array.from(fromHex(v.carriers[t.carrier]!.commitmentHex))
    const decoded = PushDrop.decode(LockingScript.fromHex(t.lockHex), 'before')
    assert.equal(decoded.lockingPublicKey.toString(), v.stateLockingKeyHex, name)
    assert.equal(decoded.fields.length, 3, name)
    const [tag, c, sig] = decoded.fields as [number[], number[], number[]]
    assert.deepEqual(tag, stateTag, name)
    assert.deepEqual(c, commitmentBytes, name)
    const key = readerLockingKey(protocol, v.stateKeyId, v.identityKeyHex)
    assert.equal(verifyFieldSignature(key, [...tag, ...c], sig), true, name)
    assert.equal(verifyFieldSignature(key, [...tag, ...c.slice(1)], sig), false, `${name}: another commitment`)
    assert.equal(Transaction.fromHex(t.txHex).outputs[0]!.lockingScript.toHex(), t.lockHex, name)
    const written = await new PushDrop(wallet).lock([stateTag, commitmentBytes], protocol, v.stateKeyId, 'anyone', true, true, 'before')
    assert.equal(written.toHex(), t.lockHex, `${name}: the SDK's PushDrop writes another lock`)
  }
})

// The kept BEEF is go-sdk's Atomic BEEF V2, and the TypeScript SDK writes V1,
// so the bytes are not compared: what matters to a host on this side is that
// it reads what a Go producer kept.
test('transactions-v1: the funding tree\'s kept Atomic BEEF parses here to the tree and its proven parent', () => {
  const { v } = chain()
  const tree = Transaction.fromAtomicBEEF(Array.from(fromHex(v.fundingTree.keptBeefHex)))
  assert.equal(tree.id('hex'), v.fundingTree.txid)
  assert.equal(tree.toHex(), v.fundingTree.txHex)
  const coin = tree.inputs[0]?.sourceTransaction
  assert.equal(coin?.id('hex'), v.coin.txid)
  assert.equal(coin?.merklePath?.toHex(), v.coin.bumpHex)
})

interface PubkeyVector {
  cases: Array<{ name: string; accept: boolean; sdkParses: boolean; keyHex: string }>
}

// The SDK takes the aliases the rule refuses, as go-sdk does, but it writes
// the key back reduced: the alias is a second encoding of the point, which
// is why the check is made on the bytes.
test('pubkeys-v1: a key is taken exactly when the independent rule takes it, and the SDK takes the aliases', () => {
  const v = vector<PubkeyVector>('pubkeys-v1.json')
  let aliases = 0
  for (const c of v.cases) {
    const b = Array.from(fromHex(c.keyHex))
    const k = strictPublicKey(b)
    assert.equal(k !== undefined, c.accept, c.name)
    if (k !== undefined) assert.equal(k.toString(), c.keyHex, `${c.name}: writes back as itself`)
    if (!c.accept && c.sdkParses && b.length === 33) {
      let sdk: PublicKey | undefined
      try {
        sdk = PublicKey.fromString(c.keyHex)
      } catch {
        sdk = undefined
      }
      if (sdk !== undefined) {
        assert.notEqual(sdk.toString(), c.keyHex, `${c.name}: the SDK writes an alias back as it read it`)
        aliases++
      }
    }
  }
  assert.ok(aliases > 0, 'the SDK takes no alias: the rule may be redundant here, but it is still the rule')
})

interface PushdropVector {
  fundingTagHex: string
  cases: Array<{ kind: string; name: string; accept: boolean; sdkDecodes: boolean; lockHex: string }>
}

test('pushdrop-v1: only the canonical encoding decodes, as a PushDrop, a funding output and a record output', () => {
  const { v } = chain()
  const pd = vector<PushdropVector>('pushdrop-v1.json')
  const fundingTag = Array.from(fromHex(pd.fundingTagHex))
  const payload = v.carriers[0]!.payloadHex
  const codec: PayloadCodec<string> = {
    inspect: (b) => (hex(b) === payload ? { kind: 'payload', payload } : { kind: 'not-payload' }),
    validate: () => undefined,
  }
  const ran: Record<string, number> = {}
  let malleable = 0
  for (const c of pd.cases) {
    const script = Script.fromHex(c.lockHex)
    assert.equal(decodeStrictPushDrop(script) !== undefined, c.accept, `${c.kind}/${c.name}`)
    if (c.kind === 'funding') assert.equal(decodeFunding(script, fundingTag) !== undefined, c.accept, `${c.kind}/${c.name}`)
    if (c.kind === 'record') assert.equal(inspectScript(script, codec).kind, c.accept ? 'carrier' : 'bad-record', `${c.kind}/${c.name}`)
    if (!c.accept && c.sdkDecodes) {
      let decodes = true
      try {
        PushDrop.decode(LockingScript.fromHex(c.lockHex), 'before')
      } catch {
        decodes = false
      }
      if (decodes) malleable++
    }
    ran[c.kind] = (ran[c.kind] ?? 0) + 1
  }
  assert.ok((ran.record ?? 0) > 0 && (ran.funding ?? 0) > 0 && (ran.state ?? 0) > 0, JSON.stringify(ran))
  assert.ok(malleable > 0, 'the SDK decodes none of the refused forms')
})
