import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PaymentExposure, arcadeVerdict, decidePayment, defaultAcceptancePolicy, paymentThreshold, zeroAcceptancePolicy } from './acceptance.js'

test('a small payment is fast and a large one held', async () => {
  const p = defaultAcceptancePolicy()
  const x = new PaymentExposure()
  assert.equal((await decidePayment(p, x, { payer: 'a', txid: '1', sats: 1_000 })).decision, 'fast')
  assert.equal((await decidePayment(p, x, { payer: 'a', txid: '2', sats: p.thresholdSats + 1 })).reason, 'above-threshold')
  assert.equal((await decidePayment(p, x, { payer: 'b', txid: '3', sats: p.thresholdSats + 1, ask: 'fast' })).decision, 'hold')
  assert.equal((await decidePayment(p, x, { payer: 'b', txid: '4', sats: 1, ask: 'hold' })).reason, 'payer-asked-hold')
  assert.equal((await decidePayment(zeroAcceptancePolicy(), x, { payer: 'b', txid: '5', sats: 1 })).decision, 'hold')
})

test('aggregation crosses the limit, per payer and in total', async () => {
  const p = { ...defaultAcceptancePolicy(), thresholdSats: 1_000, payerLimit: 2_500, totalLimit: 3_500 }
  const x = new PaymentExposure()
  const d = async (payer: string, txid: string, sats: number): Promise<string> => (await decidePayment(p, x, { payer, txid, sats })).reason
  assert.equal(await d('alice', '1', 1_000), 'at-or-below-threshold')
  assert.equal(await d('alice', '2', 1_000), 'at-or-below-threshold')
  assert.equal(await d('alice', '3', 1_000), 'payer-limit')
  assert.equal(await d('bob', '4', 1_000), 'at-or-below-threshold')
  assert.equal(await d('bob', '5', 1_000), 'total-limit')
  assert.equal(await d('carol', '6', 500), 'at-or-below-threshold')
  x.flag('carol', 'double-spent')
  assert.equal(await d('carol', '7', 1), 'payer-flagged')
  // Outside the window a charge no longer counts.
  const later = Date.now() + 2 * p.window
  assert.equal((await decidePayment(p, x, { payer: 'alice', txid: '8', sats: 1_000 }, later)).decision, 'fast')
})

test('an unknown or stale price holds every payment', async () => {
  const base = { ...defaultAcceptancePolicy(), thresholdCents: 2_500 }
  assert.equal((await paymentThreshold(base)).reason, 'price-unknown')
  assert.equal((await paymentThreshold({ ...base, price: () => Promise.reject(new Error('down')) })).reason, 'price-unknown')
  assert.equal((await paymentThreshold({ ...base, price: async () => ({ centsPerCoin: 0 }) })).reason, 'price-unknown')
  const stale = await paymentThreshold({ ...base, price: async () => ({ centsPerCoin: 10_000, at: Date.now() - 2 * base.maxPriceAge }) })
  assert.deepEqual(stale, { sats: 0, reason: 'price-stale' })
  assert.deepEqual(await paymentThreshold({ ...base, thresholdSats: 0, price: async () => ({ centsPerCoin: 10_000 }) }), { sats: 25_000_000 })
  assert.deepEqual(await paymentThreshold({ ...base, thresholdSats: 1_000, price: async () => ({ centsPerCoin: 10_000 }) }), { sats: 1_000 })
  const v = await decidePayment(base, new PaymentExposure(), { payer: 'a', txid: '1', sats: 1 })
  assert.deepEqual([v.decision, v.reason, v.threshold], ['hold', 'price-unknown', 0])
})

test('arcade statuses', () => {
  assert.equal(arcadeVerdict({ txStatus: 'SEEN_ON_NETWORK' }), 'accepted')
  assert.equal(arcadeVerdict({ txStatus: 'ACCEPTED_BY_NETWORK' }), 'accepted')
  assert.equal(arcadeVerdict({ txStatus: 'DOUBLE_SPEND_ATTEMPTED' }), 'conflict')
  assert.equal(arcadeVerdict({ txStatus: 'SEEN_ON_NETWORK', competingTxs: ['ab'] }), 'conflict')
  assert.equal(arcadeVerdict({ txStatus: 'REJECTED' }), 'refused')
  assert.equal(arcadeVerdict({ txStatus: 'RECEIVED' }), 'pending')
  assert.equal(arcadeVerdict({}), 'pending')
})
