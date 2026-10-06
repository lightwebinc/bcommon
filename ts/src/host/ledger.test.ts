/**
 * The payee ledger: every line a host wrote in the Go `payee` fixtures is
 * written again byte for byte, in both layouts; the lines with a decision
 * are held to a fixture of their own, which the Go reader reads as the same
 * payments (payee/ledger_test.go); and the receiver keeps txids and coins
 * across a restart, as the applications' copies did.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { P2PKH, PrivateKey, Utils } from '@bsv/sdk'
import { Chain, PayingWallet, payee, payeeKey } from './harness.test.js'
import { LedgerFile, LedgerReceiver, ledgerLine, readLedgerLine, spentOutpoints, type ReceivedPayment } from './ledger.js'

const fixture = (name: string): string => readFileSync(new URL(`../../../testdata/fixtures/payee/${name}`, import.meta.url), 'utf8')

/** The fixture's lines that are payments, each with its newline. */
function payments(name: string): string[] {
  return fixture(name)
    .split(/(?<=\n)/)
    .filter((l) => readLedgerLine(l) !== undefined)
}

/** A line as the payment a host would have claimed, and the time it carries. */
function received(line: string): { p: ReceivedPayment; at: number } {
  const v = JSON.parse(line) as Omit<ReceivedPayment, 'beef'> & { beef: string; at: number }
  // In the order a host's route builds it: the spread layout writes the
  // payment's own key order.
  const p: ReceivedPayment = {
    txid: v.txid,
    beef: Utils.toArray(v.beef, 'base64'),
    outputIndex: v.outputIndex,
    satoshis: v.satoshis,
    derivationPrefix: v.derivationPrefix,
    derivationSuffix: v.derivationSuffix,
    senderIdentityKey: v.senderIdentityKey,
    class: v.class,
    inputs: v.inputs,
  }
  if (v.decision !== undefined) p.decision = v.decision
  if (v.reason !== undefined) p.reason = v.reason
  return { p, at: v.at }
}

test('every version 2 line of the Go payee fixtures is written byte for byte, in both layouts', () => {
  let n = 0
  for (const name of ['ledger-v2.jsonl', 'ledger-mixed.jsonl']) {
    for (const line of payments(name)) {
      const v = JSON.parse(line) as { inputs?: unknown; at?: unknown }
      if (v.inputs === undefined || v.at === undefined) continue // version 1: a host writes version 2 now
      const { p, at } = received(line)
      assert.equal(ledgerLine(p, at), line, `${name}: payee layout`)
      assert.equal(ledgerLine(p, at, 'spread'), line, `${name}: spread layout`)
      n++
    }
  }
  assert.equal(n, 4, 'every version 2 line compared')
})

const decisions = 'ledger-decisions.jsonl'

test('a line with the host decision, in each layout, is the decisions fixture; without the decision it is the payee line', () => {
  const v2 = payments('ledger-v2.jsonl').map(received)
  const want = payments(decisions)
  const got: string[] = []
  for (const [i, { p, at }] of v2.entries()) {
    const decided: ReceivedPayment = { ...p, decision: (['fast', 'hold', 'mined'] as const)[i % 3]!, reason: i % 3 === 1 ? 'above-threshold' : 'at-or-below-threshold' }
    got.push(ledgerLine(decided, at, 'payee'), ledgerLine(decided, at, 'spread'))
    // The spread layout is the applications' own expression, byte for byte.
    assert.equal(ledgerLine(decided, at, 'spread'), JSON.stringify({ ...decided, beef: Utils.toBase64(Array.from(Uint8Array.from(decided.beef))), at }) + '\n')
    // Without a reason, the key is left out, as JSON.stringify leaves out undefined.
    assert.equal(ledgerLine({ ...decided, reason: undefined }, at).includes('"reason"'), false)
  }
  assert.deepEqual(got, want)
  for (const line of want) {
    const { decision: _d, reason: _r, ...rest } = JSON.parse(line) as Record<string, unknown>
    const { p, at } = received(JSON.stringify(rest))
    assert.ok(v2.some((x) => ledgerLine(x.p, x.at) === ledgerLine(p, at)), 'the payee line under the decision')
  }
})

test('readLedgerLine reads as a host reads at start', () => {
  assert.equal(readLedgerLine(''), undefined)
  assert.equal(readLedgerLine('{"txid":'), undefined, 'cut short by a crash')
  assert.equal(readLedgerLine('null'), undefined)
  assert.equal(readLedgerLine('{"txid":5}'), undefined)
  assert.deepEqual(readLedgerLine('{"txid":"ab","inputs":["cd.0"],"decision":"hold","at":7}'), { txid: 'ab', inputs: ['cd.0'], decision: 'hold', at: 7 })
  assert.deepEqual(readLedgerLine('{"txid":"ab","beef":"AAAA"}'), { txid: 'ab', inputs: [], beef: 'AAAA' }, 'a BEEF that does not parse claims the txid alone')
  assert.deepEqual(readLedgerLine('{"txid":"ab","decision":"other"}'), { txid: 'ab', inputs: [] })
  for (const line of payments('ledger-v1.jsonl')) {
    const v = readLedgerLine(line)!
    assert.equal(v.inputs.length, 1, 'a version 1 line has its coins read from its transaction')
  }
})

test('the payment ledger keeps every accepted payment across a restart and refuses its txids and its coins again', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bcommon-ledger-'))
  try {
    const p: ReceivedPayment = { txid: 'ab'.repeat(32), beef: [1, 1, 1, 1], outputIndex: 0, satoshis: 5, derivationPrefix: 'AA==', derivationSuffix: 'AQ==', senderIdentityKey: payeeKey, class: 'history', inputs: [`${'dd'.repeat(32)}.0`] }
    const a = new LedgerReceiver(dir, { now: () => 1_700_000_000_999 })
    assert.equal(a.claim(p), 'accepted')
    assert.equal(a.claim(p), 'replayed')
    a.close()
    assert.equal(readFileSync(join(dir, LedgerFile), 'utf8'), ledgerLine(p, 1_700_000_000))
    const b = new LedgerReceiver(dir)
    assert.equal(b.claim(p), 'replayed', 'claimed before the restart')
    assert.equal(b.claim({ ...p, txid: 'cd'.repeat(32) }), 'conflict')
    assert.equal(b.claim({ ...p, txid: 'cd'.repeat(32), inputs: [`${'ee'.repeat(32)}.1`] }), 'accepted')
    assert.equal(b.claim({ ...p, txid: 'ef'.repeat(32), inputs: [] }), 'conflict', 'a payment that names no coin is not one')
    b.close()
    const c = new LedgerReceiver(dir)
    assert.equal(c.claim({ ...p, txid: '12'.repeat(32), inputs: [`${'ee'.repeat(32)}.1`, `${'ff'.repeat(32)}.0`] }), 'conflict', 'the coins are remembered across a restart')
    c.close()
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('each layout writes its own order; a held payment is written once; fast lines are read back; a cut line is ended', () => {
  for (const layout of ['payee', 'spread'] as const) {
    const dir = mkdtempSync(join(tmpdir(), 'bcommon-ledger-'))
    try {
      writeFileSync(join(dir, LedgerFile), '{"txid":"cut')
      const p: ReceivedPayment = { txid: 'ab'.repeat(32), beef: [1, 2, 3], outputIndex: 0, satoshis: 5, derivationPrefix: 'AA==', derivationSuffix: 'AQ==', senderIdentityKey: payeeKey, class: 'history', inputs: [`${'dd'.repeat(32)}.0`] }
      const a = new LedgerReceiver(dir, { now: () => 2_000, layout })
      assert.equal(a.claim({ ...p, decision: 'hold', reason: 'above-threshold' }), 'accepted')
      assert.equal(a.claim({ ...p, decision: 'hold', reason: 'above-threshold' }), 'accepted', 'held again, not written twice')
      assert.equal(a.claim({ ...p, txid: 'cd'.repeat(32), inputs: [`${'ee'.repeat(32)}.0`], decision: 'fast', reason: 'at-or-below-threshold' }), 'accepted')
      a.close()
      const text = readFileSync(join(dir, LedgerFile), 'utf8')
      const lines = text.split('\n')
      assert.equal(lines[0], '{"txid":"cut', 'the cut line is ended, not rewritten')
      assert.equal(lines.length, 4)
      const keys = Object.keys(JSON.parse(lines[1]!) as object)
      assert.deepEqual(keys.slice(-3), layout === 'payee' ? ['at', 'decision', 'reason'] : ['decision', 'reason', 'at'])
      const b = new LedgerReceiver(dir)
      assert.equal(b.refusal(p), undefined, 'a held payment can still be answered')
      assert.deepEqual(b.fast, [{ beef: 'AQID', senderIdentityKey: payeeKey, satoshis: 5, at: 2_000 }])
      b.close()
    } finally {
      rmSync(dir, { recursive: true, force: true })
    }
  }
})

test('the ledger reads the coins of a line written before lines carried them, from its transaction', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'bcommon-ledger-'))
  try {
    const wallet = new PayingWallet(PrivateKey.fromRandom(), new Chain(96000))
    const coin = wallet.coin()
    const lock = new P2PKH().lock(payee.toAddress()).toHex()
    const first = await wallet.pay([{ satoshis: 5, lockingScript: lock }], coin)
    const second = await wallet.pay([{ satoshis: 6, lockingScript: lock }], coin)
    const line = { txid: first.id('hex'), beef: Utils.toBase64(first.toAtomicBEEF()), outputIndex: 0, satoshis: 5, derivationPrefix: 'AA==', derivationSuffix: 'AQ==', senderIdentityKey: payeeKey, class: 'history' }
    writeFileSync(join(dir, LedgerFile), JSON.stringify(line) + '\n')
    const ledger = new LedgerReceiver(dir)
    assert.deepEqual(spentOutpoints(second), [`${coin.id('hex')}.0`])
    assert.equal(ledger.claim({ ...line, beef: second.toAtomicBEEF(), txid: second.id('hex'), outputIndex: 0, satoshis: 6, inputs: spentOutpoints(second) }), 'conflict')
    ledger.close()
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})
