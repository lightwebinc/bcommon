/**
 * The carrier core under a neutral codec and derivation: a payload of magic
 * "xyp", a 33-byte identity key and a body that must not be empty, locked
 * under [2, "example records"] / "item". Carriers are minted here with the
 * SDK's own PushDrop, so the core is checked against what a wallet writes
 * rather than against its own encoder.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  LockingScript,
  P2PKH,
  PrivateKey,
  ProtoWallet,
  PushDrop,
  Transaction,
  UnlockingScript,
  Utils,
  type WalletInterface,
  type WalletProtocol,
} from '@bsv/sdk'
import {
  LockTime,
  MaxSequence,
  SigHashType,
  commitment,
  decodeCarrier,
  inspectScript,
  unlockingRefusal,
  type LockingKeyFor,
  type PayloadCodec,
  type PayloadInspection,
} from './carrier.js'
import { readerLockingKey } from './derive.js'

// TEST-ONLY keys.
const priv = PrivateKey.fromHex('42'.repeat(32))
const stranger = PrivateKey.fromHex('43'.repeat(32))
const protocol: WalletProtocol = [2, 'example records']
const keyID = 'item'
const magic = [0x78, 0x79, 0x70]

interface Item {
  identity: number[]
  body: number[]
}

const codec: PayloadCodec<Item> = {
  inspect(b: Uint8Array): PayloadInspection<Item> {
    if (b.length < 3 || b[0] !== magic[0] || b[1] !== magic[1] || b[2] !== magic[2]) return { kind: 'not-payload' }
    if (b.length < 36) return { kind: 'bad-payload', detail: 'short' }
    return { kind: 'payload', payload: { identity: Array.from(b.subarray(3, 36)), body: Array.from(b.subarray(36)) } }
  },
  validate(p: Item): void {
    if (p.body.length === 0) throw new Error('empty body')
  },
}

const lockingKeyFor: LockingKeyFor<Item> = (p) => readerLockingKey(protocol, keyID, Utils.toHex(p.identity))

const identity = (k: PrivateKey): number[] => k.toPublicKey().encode(true) as number[]
const payload = (id: number[], body: number[] = [1, 2, 3]): number[] => [...magic, ...id, ...body]

/** A signed PushDrop [payload] under signer's derivation. */
async function lock(signer: PrivateKey, fields: number[][]): Promise<LockingScript> {
  // ProtoWallet has the two calls PushDrop.lock makes, not the action surface.
  const wallet = new ProtoWallet(signer) as unknown as WalletInterface
  return new PushDrop(wallet).lock(fields, protocol, keyID, 'anyone', true, true, 'before')
}

/**
 * A funding tree under priv's derivation: outputs of the funding lock, a
 * PushDrop of one unsigned field, which the derivation's unlocker spends.
 */
async function fundingTree(signer: PrivateKey, count: number): Promise<Transaction> {
  const wallet = new ProtoWallet(signer) as unknown as WalletInterface
  const lock = await new PushDrop(wallet).lock([[0x7a, 0x7a, 0x02]], protocol, keyID, 'anyone', true, false, 'before')
  const tx = new Transaction()
  tx.addInput({ sourceTXID: '11'.repeat(32), sourceOutputIndex: 0, sequence: MaxSequence, unlockingScript: new UnlockingScript([]) })
  for (let i = 0; i < count; i++) tx.addOutput({ satoshis: 1000, lockingScript: lock })
  return tx
}

const tree = await fundingTree(priv, 4)

/**
 * A carrier whose inputs spend the tree's outputs in order, each signed by
 * the SDK's PushDrop unlocker as a wallet signs one: one input unless
 * sequences says otherwise.
 */
async function carrierTx(locks: LockingScript[], opts: { lockTime?: number; sequences?: number[] } = {}): Promise<Transaction> {
  const wallet = new ProtoWallet(priv) as unknown as WalletInterface
  const tx = new Transaction()
  tx.lockTime = opts.lockTime ?? LockTime
  for (const [i, sequence] of (opts.sequences ?? [0]).entries()) {
    tx.addInput({
      sourceTransaction: tree,
      sourceOutputIndex: i,
      sequence,
      unlockingScriptTemplate: new PushDrop(wallet).unlock(protocol, keyID, 'anyone', 'all', false),
    })
  }
  for (const l of locks) tx.addOutput({ satoshis: 1, lockingScript: l })
  await tx.sign()
  return tx
}

/** tx with input 0's unlocking script replaced by the given bytes. */
function withUnlocking(tx: Transaction, bytes: number[]): Transaction {
  const out = Transaction.fromHex(tx.toHex())
  out.inputs[0]!.unlockingScript = UnlockingScript.fromBinary(bytes)
  return out
}

/** A canonical unlocking script's signature with S replaced by n - S. */
function flipS(unlocking: number[]): number[] {
  const sig = unlocking.slice(1)
  const der = sig.slice(0, -1)
  const lenR = der[3]!
  const r = der.slice(4, 4 + lenR)
  let s = 0n
  for (const b of der.slice(6 + lenR)) s = (s << 8n) | BigInt(b)
  let f = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n - s
  const fb: number[] = []
  while (f > 0n) {
    fb.unshift(Number(f & 0xffn))
    f >>= 8n
  }
  if ((fb[0]! & 0x80) !== 0) fb.unshift(0)
  const body = [0x02, r.length, ...r, 0x02, fb.length, ...fb]
  const out = [0x30, body.length, ...body, sig[sig.length - 1]!]
  return [out.length, ...out]
}

test('the frozen finality values', () => {
  assert.equal(LockTime, 4102444800)
  assert.equal(MaxSequence, 0xffffffff)
})

test('a carrier decodes with its payload, key and commitment', async () => {
  const bytes = payload(identity(priv))
  const other = await lock(priv, [[0x61, 0x62, 0x63]])
  const tx = await carrierTx([other, await lock(priv, [bytes])])
  const c = decodeCarrier(tx, codec, lockingKeyFor)
  assert.ok(typeof c !== 'string', `refused: ${String(c)}`)
  assert.equal(c.outputIndex, 1, 'a two-field PushDrop the codec does not take is skipped')
  assert.deepEqual(Array.from(c.payloadBytes), bytes)
  assert.deepEqual(c.payload.body, [1, 2, 3])
  assert.equal(c.lockingKey.toString(), lockingKeyFor(c.payload).toString())
  assert.deepEqual(c.c, commitment(tx))
  assert.deepEqual(c.c, tx.hash())
  assert.notEqual(Utils.toHex(c.c), tx.id('hex'), 'hash byte order, not the display id')
})

test('each rule refuses by its own code', async () => {
  const good = await lock(priv, [payload(identity(priv))])
  const refuse = async (tx: Promise<Transaction> | Transaction): Promise<unknown> => decodeCarrier(await tx, codec, lockingKeyFor)
  assert.equal(await refuse(carrierTx([good], { lockTime: LockTime - 1 })), 'mineable')
  assert.equal(await refuse(carrierTx([good], { sequences: [0, MaxSequence] })), 'mineable', 'one final input')
  assert.equal(await refuse(carrierTx([good], { sequences: [] })), 'other', 'no inputs')
  assert.equal(await refuse(carrierTx([await lock(priv, [payload(identity(priv), [])])])), 'bad-record', 'the payload rules')
  assert.equal(await refuse(carrierTx([await lock(priv, [[...magic, 1]])])), 'bad-record', 'a payload that does not decode')
  assert.equal(await refuse(carrierTx([good, good])), 'bad-record', 'two record outputs')
  assert.equal(await refuse(carrierTx([await lock(stranger, [payload(identity(priv))])])), 'bad-lock', 'locked by another key')
  assert.equal(await refuse(carrierTx([await lock(priv, [payload(new Array(33).fill(0))])])), 'bad-lock', 'an identity with no derivation')
  assert.equal(await refuse(carrierTx([new P2PKH().lock(priv.toAddress())])), 'not-pushdrop')
  assert.equal(await refuse(carrierTx([])), 'not-pushdrop', 'no outputs')

  const badSig = await carrierTx([good])
  const chunks = good.chunks.map((c) => ({ ...c, data: c.data === undefined ? undefined : [...c.data] }))
  chunks[3]!.data![10] = (chunks[3]!.data![10] ?? 0) ^ 0x01
  badSig.outputs[0]!.lockingScript = new LockingScript(chunks)
  assert.equal(await refuse(badSig), 'bad-sig')
})

// The order is the contract: a carrier that breaks two rules is refused for
// the first of payload, finality, lock, signature.
test('a carrier breaking two rules is refused for the first', async () => {
  const invalid = await lock(priv, [payload(identity(priv), [])])
  assert.equal(decodeCarrier(await carrierTx([invalid], { lockTime: 0 }), codec, lockingKeyFor), 'bad-record')
  const foreign = await lock(stranger, [payload(identity(priv))])
  assert.equal(decodeCarrier(await carrierTx([foreign], { lockTime: 0 }), codec, lockingKeyFor), 'mineable')
  const chunks = foreign.chunks.map((c) => ({ ...c, data: c.data === undefined ? undefined : [...c.data] }))
  chunks[3]!.data![10] = (chunks[3]!.data![10] ?? 0) ^ 0x01
  assert.equal(decodeCarrier(await carrierTx([new LockingScript(chunks)]), codec, lockingKeyFor), 'bad-lock')
})

test('inspectScript tells a record output from anything else', async () => {
  const good = await lock(priv, [payload(identity(priv))])
  assert.equal(inspectScript(good, codec).kind, 'carrier')
  assert.deepEqual(inspectScript(await lock(priv, [[...magic, 1]]), codec), { kind: 'bad-record', detail: 'short' })
  assert.equal(inspectScript(await lock(priv, [[0x61, 0x62, 0x63]]), codec).kind, 'not-record', 'another magic')
  assert.equal(inspectScript(await lock(priv, [payload(identity(priv)), [1]]), codec).kind, 'not-record', 'three fields')
  assert.equal(inspectScript(new P2PKH().lock(priv.toAddress()), codec).kind, 'not-record')
  assert.equal(inspectScript(new LockingScript([]), codec).kind, 'not-record')
})

test('a codec fault propagates rather than refusing', async () => {
  const faulty: PayloadCodec<Item> = {
    inspect: () => {
      throw new TypeError('codec fault')
    },
    validate: () => undefined,
  }
  const tx = await carrierTx([await lock(priv, [payload(identity(priv))])])
  assert.throws(() => decodeCarrier(tx, faulty, lockingKeyFor), /codec fault/)
})

test('the frozen sighash type', () => {
  assert.equal(SigHashType, 0x41)
})

// Anyone who sees a carrier can rewrite its unlocking script without the
// key; each rewrite is refused, as a hand-built carrier's mistakes are, and
// the original passes.
test('only the one canonical signature push is taken', async () => {
  const good = await carrierTx([await lock(priv, [payload(identity(priv))])])
  const u = good.inputs[0]!.unlockingScript!.toBinary()
  const sig = u.slice(1)
  const der = sig.slice(0, -1)
  const lenR = der[3]!
  const r = der.slice(4, 4 + lenR)
  const s = der.slice(6 + lenR)
  const push = (b: number[]): number[] => [b.length, ...b]
  const intDER = (ri: number[], si: number[]): number[] => {
    const body = [0x02, ri.length, ...ri, 0x02, si.length, ...si]
    return [0x30, body.length, ...body]
  }
  const withDER = (d: number[]): number[] => push([...d, SigHashType])
  assert.equal(unlockingRefusal(good), undefined)
  assert.ok(typeof decodeCarrier(good, codec, lockingKeyFor) !== 'string')
  const rows: Array<[string, number[]]> = [
    ['high S', flipS(u)],
    ['OP_PUSHDATA1 for a short push', [0x4c, sig.length, ...sig]],
    ['OP_PUSHDATA2 for a short push', [0x4d, sig.length, 0, ...sig]],
    ['OP_0 before the signature', [0x00, ...u]],
    ['the signature pushed twice', [...u, ...u]],
    ['OP_NOP after the signature', [...u, 0x61]],
    ['OP_NOP before the signature', [0x61, ...u]],
    ['truncated', u.slice(0, -1)],
    ['R padded', withDER(intDER([0, ...r], s))],
    ['S padded', withDER(intDER(r, [0, ...s]))],
    ['R negative', withDER(intDER([0x80, 1], s))],
    ['S negative', withDER(intDER(r, [0x80, 1]))],
    ['R empty', withDER(intDER([], s))],
    ['sequence length one more', withDER([0x30, der[1]! + 1, ...der.slice(2)])],
    ['a byte after S', withDER([...der, 0])],
    ['S zero', withDER(intDER(r, [0]))],
    ['R zero', withDER(intDER([0], s))],
    ['sighash ALL without FORKID', push([...der, 0x01])],
    ['sighash ALL|ANYONECANPAY|FORKID', push([...der, 0xc1])],
    ['no sighash byte', push(der)],
    ['empty', []],
  ]
  for (const [name, bytes] of rows) {
    const tx = withUnlocking(good, bytes)
    assert.notEqual(tx.id('hex'), good.id('hex'), `${name}: the txid moves`)
    assert.equal(unlockingRefusal(tx), 'non-canonical-unlocking', name)
    assert.equal(decodeCarrier(tx, codec, lockingKeyFor), 'non-canonical-unlocking', name)
  }
  const two = await carrierTx([await lock(priv, [payload(identity(priv))])], { sequences: [0, 0] })
  assert.equal(unlockingRefusal(two), 'non-canonical-unlocking', 'two inputs')
  assert.equal(decodeCarrier(two, codec, lockingKeyFor), 'non-canonical-unlocking', 'two inputs')
  const unsigned = Transaction.fromHex(good.toHex())
  unsigned.inputs[0]!.unlockingScript = undefined
  assert.equal(unlockingRefusal(unsigned), 'non-canonical-unlocking', 'no unlocking script')
})

// The unlocking script is checked after the payload's rules and finality,
// and before the lock and the field signature.
test('the unlocking script is refused in its place in the order', async () => {
  const flip = (tx: Transaction): Transaction => withUnlocking(tx, flipS(tx.inputs[0]!.unlockingScript!.toBinary()))
  const invalid = await carrierTx([await lock(priv, [payload(identity(priv), [])])])
  assert.equal(decodeCarrier(flip(invalid), codec, lockingKeyFor), 'bad-record')
  const mineable = await carrierTx([await lock(priv, [payload(identity(priv))])], { lockTime: 0 })
  assert.equal(decodeCarrier(flip(mineable), codec, lockingKeyFor), 'mineable')
  const finalSecond = await carrierTx([await lock(priv, [payload(identity(priv))])], { sequences: [0, MaxSequence] })
  assert.equal(decodeCarrier(finalSecond, codec, lockingKeyFor), 'mineable')
  const foreign = await carrierTx([await lock(stranger, [payload(identity(priv))])])
  assert.equal(decodeCarrier(flip(foreign), codec, lockingKeyFor), 'non-canonical-unlocking')
})

// The SDK's wallet signs with a low S in strict DER, so every carrier it
// signs passes. Many signings, each over another payload and so another
// nonce, would meet a high S about half the time if it did not.
test('carriers the SDK signs are canonical', async () => {
  for (let i = 0; i < 200; i++) {
    const tx = await carrierTx([await lock(priv, [payload(identity(priv), [i & 0xff, i >> 8, 7])])])
    assert.equal(unlockingRefusal(tx), undefined, `signing ${i}`)
    assert.ok(typeof decodeCarrier(tx, codec, lockingKeyFor) !== 'string', `signing ${i}`)
  }
})
