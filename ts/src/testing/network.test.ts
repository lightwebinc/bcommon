import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import type { AddressInfo } from 'node:net'
import { LockingScript, MerklePath, Transaction, UnlockingScript } from '@bsv/sdk'
import { TestNetwork, serveNetwork } from './network.js'

/** A chain that mines a transaction as the only one of its block. */
const chain = {
  mine(tx: Transaction): Transaction {
    tx.merklePath = new MerklePath(100, [[{ offset: 0, hash: tx.id('hex'), txid: true }]])
    return tx
  },
}

const parent = new Transaction()
parent.addOutput({ satoshis: 1000, lockingScript: LockingScript.fromHex('51') })

function spend(sats: number): Transaction {
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex('51') })
  tx.addOutput({ satoshis: sats, lockingScript: LockingScript.fromHex('51') })
  return tx
}

test('arcade in memory: seen, a competing spend, a spend elsewhere, mined with a proof', async () => {
  const net = new TestNetwork(chain)
  const a = spend(900)
  const b = spend(800)
  const sa = await net.submit(Uint8Array.from(a.toEF()))
  assert.equal(sa.txStatus, 'SEEN_ON_NETWORK')
  const sb = await net.submit(Uint8Array.from(b.toEF()))
  assert.deepEqual([sb.txStatus, sb.competingTxs], ['DOUBLE_SPEND_ATTEMPTED', [a.id('hex')]])
  assert.equal(await net.spender(parent.id('hex'), 0), a.id('hex'))
  net.spendElsewhere(`${parent.id('hex')}.1`)
  assert.equal(await net.spender(parent.id('hex'), 1), 'ab'.repeat(32))
  net.spendUnknown = true
  await assert.rejects(net.spender(parent.id('hex'), 0), /NOT_FOUND/)
  assert.equal(await net.proof(a.id('hex')), undefined)
  net.mine(a.id('hex'))
  assert.equal((await net.status(a.id('hex')))?.txStatus, 'MINED')
  assert.ok((await net.proof(a.id('hex'))) !== undefined)
  net.mode = 'down'
  await assert.rejects(net.submit(Uint8Array.from(spend(700).toEF())), /unreachable/)
  assert.deepEqual(net.sent, [a.id('hex'), b.id('hex')])
})

test('the network over HTTP, as a host reads it', async () => {
  const net = new TestNetwork(chain)
  const a = spend(900)
  const server = createServer((req, res) => {
    void serveNetwork(net, req, res).then((served) => {
      if (!served) {
        res.writeHead(404)
        res.end()
      }
    })
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  const base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`
  try {
    const sent = await fetch(`${base}/arcade/tx`, { method: 'POST', body: Uint8Array.from(a.toEF()) })
    assert.equal(((await sent.json()) as { txStatus: string }).txStatus, 'SEEN_ON_NETWORK')
    assert.equal((await fetch(`${base}/arcade/tx/${a.id('hex')}`)).status, 200)
    assert.equal((await fetch(`${base}/arcade/tx/${'00'.repeat(32)}`)).status, 404)
    const utxos = (await (await fetch(`${base}/api/v1/utxos/${parent.id('hex')}/json`)).json()) as Array<{ vout: number; status: string }>
    assert.deepEqual(utxos.slice(0, 2).map((u) => u.status), ['SPENT', 'OK'])
    assert.equal((await fetch(`${base}/api/v1/merkle_proof/${a.id('hex')}`)).status, 404)
    net.mine(a.id('hex'))
    const mp = await fetch(`${base}/api/v1/merkle_proof/${a.id('hex')}`)
    assert.equal(mp.status, 200)
    assert.equal(MerklePath.fromBinary(Array.from(new Uint8Array(await mp.arrayBuffer()))).blockHeight, 100)
    assert.equal((await fetch(`${base}/other`)).status, 404)
  } finally {
    server.close()
  }
})
