/**
 * The writer against the Go-generated transactions-v1.json: with the
 * vector's key in a ProtoWallet (the wallet surface a BRC-100 wallet
 * offers), the funding lock, every carrier and the self-paying sweep come
 * out byte for byte as the Go library and the independent generator wrote
 * them, and what the writer mints passes the reader's own carrier checks.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { PrivateKey, ProtoWallet, Transaction, type WalletProtocol } from '@bsv/sdk'
import { mineableRefusal, unlockingRefusal } from './carrier.js'
import { decodeFunding } from './funding.js'
import { fromHex } from './testing/index.js'
import { feeFor, fundingLock, mintCarrier, sweep, type Derivation } from './writer.js'

interface V {
  privateKeyHex: string
  originator: string
  securityLevel: number
  protocol: string
  objectKeyId: string
  objectLockingKeyHex: string
  fundingTagHex: string
  satPerByte: number
  floor: number
  fundingLockHex: string
  fundingTree: { txHex: string }
  carriers: Array<{ vout: number; payloadHex: string; lockHex: string; txid: string; txHex: string }>
  sweep: { vouts: number[]; feeSats: number; txid: string; txHex: string }
}

const v = JSON.parse(readFileSync(new URL('../../testdata/vectors/transactions-v1.json', import.meta.url), 'utf8')) as V
const w = new ProtoWallet(PrivateKey.fromHex(v.privateKeyHex))
const d: Derivation = { protocol: [v.securityLevel as WalletProtocol[0], v.protocol], keyID: v.objectKeyId }
const tag = fromHex(v.fundingTagHex)
const tree = Transaction.fromHex(v.fundingTree.txHex)

test('writer: the funding lock is the Go producer\'s', async () => {
  assert.equal((await fundingLock(w, v.originator, d, tag)).toHex(), v.fundingLockHex)
  assert.equal(decodeFunding((await fundingLock(w, v.originator, d, tag)) as never, Array.from(tag))?.toString(), v.objectLockingKeyHex)
  await assert.rejects(fundingLock(w, v.originator, d, new Uint8Array(0)), /empty funding tag/)
})

test('writer: every carrier is the Go producer\'s, byte for byte, and passes the reader\'s checks', async () => {
  assert.ok(v.carriers.length > 0)
  for (const [i, c] of v.carriers.entries()) {
    const tx = await mintCarrier(w, v.originator, d, fromHex(c.payloadHex), tree, c.vout)
    assert.equal(tx.outputs[0]!.lockingScript.toHex(), c.lockHex, `carrier ${i} lock`)
    assert.equal(tx.toHex(), c.txHex, `carrier ${i}`)
    assert.equal(tx.id('hex'), c.txid, `carrier ${i} txid`)
    assert.equal(mineableRefusal(tx), undefined, `carrier ${i} is unmineable`)
    assert.equal(unlockingRefusal(tx), undefined, `carrier ${i} unlocking`)
  }
  await assert.rejects(mintCarrier(w, v.originator, d, new Uint8Array([1]), tree, tree.outputs.length), /out of range/)
})

test('writer: the self-paying sweep is the Go producer\'s, its fee settled by the same loop', async () => {
  const fees = { sats: v.satPerByte, bytes: 1, floor: v.floor }
  const tx = await sweep(w, v.originator, d, tag, tree, v.sweep.vouts, fees)
  assert.equal(tx.toHex(), v.sweep.txHex)
  assert.equal(tx.id('hex'), v.sweep.txid)
  const inSats = v.sweep.vouts.reduce((n, i) => n + tree.outputs[i]!.satoshis!, 0)
  assert.equal(inSats - tx.outputs[0]!.satoshis!, v.sweep.feeSats)
})

test('writer: fees round up, keep the floor and refuse above the maximum', () => {
  assert.equal(feeFor({ sats: 100, bytes: 1000, floor: 1 }, 1001), 101)
  assert.equal(feeFor({ sats: 100, bytes: 1000, floor: 100 }, 10), 100)
  assert.throws(() => feeFor({ sats: 1, bytes: 1, floor: 1, max: 50 }, 100), /above the maximum/)
  assert.throws(() => feeFor({ sats: 1, bytes: 0, floor: 1 }, 1), /bad fee rate/)
})
