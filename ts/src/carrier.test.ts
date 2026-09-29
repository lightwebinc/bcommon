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
  commitment,
  decodeCarrier,
  inspectScript,
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

function carrierTx(locks: LockingScript[], opts: { lockTime?: number; sequences?: number[] } = {}): Transaction {
  const tx = new Transaction()
  tx.lockTime = opts.lockTime ?? LockTime
  for (const [i, sequence] of (opts.sequences ?? [0]).entries()) {
    tx.addInput({ sourceTXID: '11'.repeat(32), sourceOutputIndex: i, sequence, unlockingScript: new UnlockingScript([]) })
  }
  for (const l of locks) tx.addOutput({ satoshis: 1, lockingScript: l })
  return tx
}

test('the frozen finality values', () => {
  assert.equal(LockTime, 4102444800)
  assert.equal(MaxSequence, 0xffffffff)
})

test('a carrier decodes with its payload, key and commitment', async () => {
  const bytes = payload(identity(priv))
  const other = await lock(priv, [[0x61, 0x62, 0x63]])
  const tx = carrierTx([other, await lock(priv, [bytes])])
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
  const refuse = (tx: Transaction): unknown => decodeCarrier(tx, codec, lockingKeyFor)
  assert.equal(refuse(carrierTx([good], { lockTime: LockTime - 1 })), 'mineable')
  assert.equal(refuse(carrierTx([good], { sequences: [0, MaxSequence] })), 'mineable', 'one final input')
  assert.equal(refuse(carrierTx([good], { sequences: [] })), 'other', 'no inputs')
  assert.equal(refuse(carrierTx([await lock(priv, [payload(identity(priv), [])])])), 'bad-record', 'the payload rules')
  assert.equal(refuse(carrierTx([await lock(priv, [[...magic, 1]])])), 'bad-record', 'a payload that does not decode')
  assert.equal(refuse(carrierTx([good, good])), 'bad-record', 'two record outputs')
  assert.equal(refuse(carrierTx([await lock(stranger, [payload(identity(priv))])])), 'bad-lock', 'locked by another key')
  assert.equal(refuse(carrierTx([await lock(priv, [payload(new Array(33).fill(0))])])), 'bad-lock', 'an identity with no derivation')
  assert.equal(refuse(carrierTx([new P2PKH().lock(priv.toAddress())])), 'not-pushdrop')
  assert.equal(refuse(carrierTx([])), 'not-pushdrop', 'no outputs')

  const badSig = carrierTx([good])
  const chunks = good.chunks.map((c) => ({ ...c, data: c.data === undefined ? undefined : [...c.data] }))
  chunks[3]!.data![10] = (chunks[3]!.data![10] ?? 0) ^ 0x01
  badSig.outputs[0]!.lockingScript = new LockingScript(chunks)
  assert.equal(refuse(badSig), 'bad-sig')
})

// The order is the contract: a carrier that breaks two rules is refused for
// the first of payload, finality, lock, signature.
test('a carrier breaking two rules is refused for the first', async () => {
  const invalid = await lock(priv, [payload(identity(priv), [])])
  assert.equal(decodeCarrier(carrierTx([invalid], { lockTime: 0 }), codec, lockingKeyFor), 'bad-record')
  const foreign = await lock(stranger, [payload(identity(priv))])
  assert.equal(decodeCarrier(carrierTx([foreign], { lockTime: 0 }), codec, lockingKeyFor), 'mineable')
  const chunks = foreign.chunks.map((c) => ({ ...c, data: c.data === undefined ? undefined : [...c.data] }))
  chunks[3]!.data![10] = (chunks[3]!.data![10] ?? 0) ^ 0x01
  assert.equal(decodeCarrier(carrierTx([new LockingScript(chunks)]), codec, lockingKeyFor), 'bad-lock')
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
  const tx = carrierTx([await lock(priv, [payload(identity(priv))])])
  assert.throws(() => decodeCarrier(tx, faulty, lockingKeyFor), /codec fault/)
})
