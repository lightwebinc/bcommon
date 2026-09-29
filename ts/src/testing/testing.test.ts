/**
 * The testing entry point's helpers. An application's tests stand on them,
 * so a helper that simulated the engine wrongly would pass that
 * application's tests against an engine that does not exist: the payloads
 * and the call order are checked here against what the engine does.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { P2PKH, PrivateKey, Transaction, UnlockingScript } from '@bsv/sdk'
import type { LookupService, OutputAdmittedByTopic, OutputSpent } from '../engine-types.js'
import {
  atomicOf,
  beefOf,
  countingHost,
  engineOrder,
  fakeStorage,
  fill,
  fromHex,
  minter,
  row,
  sha256,
  spends,
  toHex,
  wireParent,
} from './index.js'

// TEST-ONLY key.
const priv = PrivateKey.fromHex('42'.repeat(32))
const topic = 'tm_example'

/** A parent with three outputs, and a child spending outputs 2 and 0 of it with a signature-shaped unlock each. */
function pair(): { parent: Transaction; child: Transaction } {
  const parent = new Transaction()
  parent.addInput({ sourceTXID: '11'.repeat(32), sourceOutputIndex: 0, sequence: 0xffffffff, unlockingScript: new UnlockingScript([]) })
  for (const sats of [1000, 2000, 3000]) parent.addOutput({ satoshis: sats, lockingScript: new P2PKH().lock(priv.toAddress()) })
  const child = new Transaction()
  child.addInput({ sourceTXID: parent.id('hex'), sourceOutputIndex: 2, sequence: 7, unlockingScript: new UnlockingScript([{ op: 1, data: [0xaa] }]) })
  child.addInput({ sourceTXID: parent.id('hex'), sourceOutputIndex: 0, sequence: 0, unlockingScript: new UnlockingScript([{ op: 1, data: [0xbb] }]) })
  child.addOutput({ satoshis: 5000, lockingScript: new P2PKH().lock(priv.toAddress()) })
  return { parent, child }
}

type Call = { admitted: OutputAdmittedByTopic } | { spent: OutputSpent }

/** A lookup service that records its callbacks, synchronously or after a turn of the event loop. */
function recorder(
  admissionMode: LookupService['admissionMode'],
  spendNotificationMode: LookupService['spendNotificationMode'],
  async = false,
): { ls: LookupService; calls: Call[] } {
  const calls: Call[] = []
  const later = (c: Call): Promise<void> | void =>
    async
      ? new Promise((resolve) =>
          setImmediate(() => {
            calls.push(c)
            resolve()
          }),
        )
      : void calls.push(c)
  const ls: LookupService = {
    admissionMode,
    spendNotificationMode,
    outputAdmittedByTopic: (payload) => later({ admitted: payload }),
    outputSpent: (payload) => later({ spent: payload }),
    outputEvicted: () => undefined,
    lookup: async () => [],
    getDocumentation: async () => '',
    getMetaData: async () => ({ name: 'example', shortDescription: '' }),
  }
  return { ls, calls }
}

test('hex, fill and sha256', () => {
  assert.deepEqual([...fromHex('00ff10')], [0x00, 0xff, 0x10])
  assert.equal(toHex(Uint8Array.of(0xde, 0xad)), 'dead')
  assert.equal(toHex([0xbe, 0xef]), 'beef')
  assert.deepEqual([...fill(0x42, 3)], [0x42, 0x42, 0x42])
  assert.equal(fill(0x01).length, 32)
  assert.equal(toHex(sha256(new Uint8Array())), 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855')
})

test('countingHost counts by name and label set, whatever the label order', () => {
  const host = countingHost()
  assert.equal(host.count('refused_total', { reason: 'x' }), -1, 'never touched')
  host.metrics.preset('refused_total', { reason: 'x', topic })
  assert.equal(host.count('refused_total', { topic, reason: 'x' }), 0, 'preset')
  host.metrics.inc('refused_total', { topic, reason: 'x' })
  host.metrics.inc('refused_total', { reason: 'x', topic }, 2)
  host.metrics.preset('refused_total', { reason: 'x', topic })
  assert.equal(host.count('refused_total', { reason: 'x', topic }), 3, 'a preset does not reset')
  assert.equal(host.count('refused_total', { reason: 'y', topic }), -1)
  host.metrics.gauge('held', () => 7)
  assert.equal(host.gauges.get('held')?.(), 7)
  host.log('mounted', { version: '1' })
  assert.deepEqual(host.logged, [{ msg: 'mounted', extra: { version: '1' } }])
})

test('row, spends, fakeStorage and wireParent describe a transaction as the engine stores it', async () => {
  const { parent, child } = pair()
  const r = row(parent, 1, topic, true, { consumedBy: [{ txid: child.id('hex'), outputIndex: 0 }] })
  assert.equal(r.txid, parent.id('hex'))
  assert.equal(r.outputIndex, 1)
  assert.equal(r.satoshis, 2000)
  assert.equal(r.topic, topic)
  assert.equal(r.spent, true)
  assert.deepEqual(r.outputScript, parent.outputs[1]!.lockingScript.toBinary())
  assert.deepEqual(r.outputsConsumed, [])
  assert.throws(() => row(parent, 3, topic), /no output 3/)

  assert.deepEqual(spends(child), [
    { txid: parent.id('hex'), outputIndex: 2 },
    { txid: parent.id('hex'), outputIndex: 0 },
  ])
  const storage = fakeStorage([r])
  assert.deepEqual(await storage.findOutput(parent.id('hex'), 1), r)
  assert.equal(await storage.findOutput(parent.id('hex'), 0), null)

  wireParent(child, parent)
  assert.equal(child.inputs[0]!.sourceTransaction, parent)
  assert.equal(child.inputs[1]!.sourceTransaction, parent)
  const stranger = new Transaction()
  stranger.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(priv.toAddress()) })
  wireParent(parent, stranger)
  assert.equal(parent.inputs[0]!.sourceTransaction, undefined, 'an input spending something else is left alone')
})

test('beefOf and atomicOf carry the transaction and the parents wired to it', () => {
  const { parent, child } = pair()
  wireParent(child, parent)
  assert.equal(Transaction.fromAtomicBEEF(atomicOf(child)).id('hex'), child.id('hex'))
  const tx = Transaction.fromBEEF(beefOf(child))
  assert.equal(tx.id('hex'), child.id('hex'))
  assert.equal(tx.inputs[0]!.sourceTransaction?.id('hex'), parent.id('hex'))
})

test('minter is a wallet over the key, and its identity is the key\'s', async () => {
  const m = minter('42'.repeat(32))
  assert.equal(m.identityKeyHex, priv.toPublicKey().toString())
  const { publicKey } = await m.wallet.getPublicKey({ identityKey: true })
  assert.equal(publicKey, m.identityKeyHex)
})

test('a synchronous service is driven synchronously, in whole-tx and txid mode, in output order', () => {
  const { parent, child } = pair()
  const { ls, calls } = recorder('whole-tx', 'txid')
  const e = engineOrder(ls, topic)
  assert.equal(e.spend(parent, child, 2), undefined)
  assert.equal(e.admitAll(child), undefined)
  assert.equal(e.admit(parent, 1), undefined)
  assert.equal(e.spendInput(child, child, 1), undefined)
  assert.deepEqual(calls, [
    { spent: { mode: 'txid', txid: parent.id('hex'), outputIndex: 2, topic, spendingTxid: child.id('hex') } },
    { admitted: { mode: 'whole-tx', atomicBEEF: atomicOf(child), outputIndex: 0, topic } },
    { admitted: { mode: 'whole-tx', atomicBEEF: atomicOf(parent), outputIndex: 1, topic } },
    { spent: { mode: 'txid', txid: parent.id('hex'), outputIndex: 0, topic, spendingTxid: child.id('hex') } },
  ])
  assert.throws(() => e.admit(child, 1), /no output 1/)
  const valueless = new Transaction()
  valueless.addOutput({ lockingScript: new P2PKH().lock(priv.toAddress()), change: true })
  assert.throws(() => e.admit(valueless), /has no satoshis/)
})

test('an asynchronous service is awaited call by call, so the order holds', async () => {
  const { parent } = pair()
  const { ls, calls } = recorder('whole-tx', 'none', true)
  const e = engineOrder(ls, topic)
  const step = e.admitAll(parent)
  assert.ok(step instanceof Promise)
  assert.equal(calls.length, 0, 'nothing is recorded before the first call settles')
  await step
  assert.deepEqual(
    calls.map((c) => ('admitted' in c ? c.admitted.outputIndex : -1)),
    [0, 1, 2],
  )
  await e.spend(parent, parent, 1)
  assert.deepEqual(calls[3], { spent: { mode: 'none', txid: parent.id('hex'), outputIndex: 1, topic } })
})

test('the payload follows the service\'s declared modes', () => {
  const { parent, child } = pair()
  {
    const { ls, calls } = recorder('locking-script', 'script')
    const e = engineOrder(ls, topic)
    e.admit(parent, 2)
    e.spend(parent, child, 0)
    assert.deepEqual(calls, [
      {
        admitted: {
          mode: 'locking-script',
          txid: parent.id('hex'),
          outputIndex: 2,
          topic,
          satoshis: 3000,
          lockingScript: parent.outputs[2]!.lockingScript,
        },
      },
      {
        spent: {
          mode: 'script',
          txid: parent.id('hex'),
          outputIndex: 0,
          topic,
          spendingTxid: child.id('hex'),
          inputIndex: 1,
          unlockingScript: child.inputs[1]!.unlockingScript!,
          sequenceNumber: 0,
        },
      },
    ])
    assert.throws(() => e.spend(parent, child, 1), /has no signed input spending/, 'the child does not spend output 1')
  }
  {
    const { ls, calls } = recorder('whole-tx', 'whole-tx')
    engineOrder(ls, topic).spend(parent, child, 2)
    assert.deepEqual(calls, [{ spent: { mode: 'whole-tx', txid: parent.id('hex'), outputIndex: 2, topic, spendingAtomicBEEF: atomicOf(child) } }])
  }
  {
    const { ls, calls } = recorder('whole-tx', 'txid')
    delete (ls as { outputSpent?: unknown }).outputSpent
    assert.equal(engineOrder(ls, topic).spend(parent, child, 2), undefined)
    assert.deepEqual(calls, [], 'a service without outputSpent is not told')
  }
})
