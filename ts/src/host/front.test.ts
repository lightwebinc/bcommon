/**
 * The terms route over real HTTP, as the applications' hosts tested their
 * copies: the terms document, free questions with and without BRC-104, a
 * priced question answered 402 and then answered once paid by the SDK's
 * own AuthFetch client, every refusal of a payment, the handshake budget,
 * replay refusal, the response budget in both placements, bounded
 * sessions, and acceptance on the paid path.
 */
import { test, after } from 'node:test'
import assert from 'node:assert/strict'
import type { AddressInfo } from 'node:net'
import { createServer, request, type IncomingHttpHeaders } from 'node:http'
import { AuthFetch, P2PKH, PrivateKey, ProtoWallet, PublicKey, Random, Transaction, Utils, createNonce, type CreateActionArgs, type CreateActionResult, type PeerSession, type WalletInterface } from '@bsv/sdk'
import { ArcadeHttp, BroadcastRefused } from './accept.js'
import { parsePrices, termsDocument, termsPath } from './front.js'
import { Chain, Classes, PayingWallet, Protocol, Service, ask, fromNowhere, payee, payeeKey, post, quietly, rig, servers, type Rig } from './harness.test.js'
import type { ReceivedPayment } from './ledger.js'
import { BoundedSessions } from './sessions.js'

after(() => {
  for (const s of servers) s.close()
})

const prices = (raw: string | undefined): Map<string, number> => parsePrices(raw, Classes, 'EXAMPLE_PRICES')
const payeeWallet = new ProtoWallet(payee)

test('prices: priceable classes only, once each, 0 to 2^53 - 1, a class with its -after form; the terms document lists them in class order', () => {
  assert.deepEqual([...prices('history-after=7, history=5')], [['history-after', 7], ['history', 5]])
  assert.equal(prices(undefined).size, 0)
  assert.equal(prices(' ').size, 0)
  for (const [bad, msg] of [
    ['list=1', 'EXAMPLE_PRICES: list is a free class and never has a price'],
    ['history', 'EXAMPLE_PRICES: "history" is not <class>=<satoshis>'],
    ['nosuch=1', 'EXAMPLE_PRICES: "nosuch=1" is not <class>=<satoshis>'],
    ['history=-1', 'EXAMPLE_PRICES: history: "-1" is not an integer from 0 to 2^53 - 1'],
    ['history=01', 'EXAMPLE_PRICES: history: "01" is not an integer from 0 to 2^53 - 1'],
    ['history=1.5', 'EXAMPLE_PRICES: history: "1.5" is not an integer from 0 to 2^53 - 1'],
    ['history=9007199254740992', 'EXAMPLE_PRICES: history: "9007199254740992" is not an integer from 0 to 2^53 - 1'],
    ['history=1,history=2', 'EXAMPLE_PRICES names history twice'],
    ['history=5', 'EXAMPLE_PRICES: history and history-after are priced together or not at all'],
    ['history=0,history-after=5', 'EXAMPLE_PRICES: history and history-after are priced together or not at all'],
  ] as const) {
    assert.throws(() => prices(bad), { message: msg }, bad)
  }
  for (const good of ['history=5,history-after=6', 'history=0', 'history-after=0', 'history=0,history-after=0']) prices(good)
  assert.deepEqual(termsDocument(Service, Classes, prices('history-after=7,history=5')), {
    service: Service,
    terms: 1,
    classes: [
      { class: 'history', satoshis: 5 },
      { class: 'history-after', satoshis: 7 },
    ],
  })
  assert.equal(termsPath(Service), '/ls_example/terms')
})

test('the terms document, free questions without authentication, and refusals as errors', async () => {
  const r = await rig()
  const terms = await fetch(`${r.url}${termsPath(Service)}`)
  assert.equal(terms.status, 200)
  assert.deepEqual(await terms.json(), termsDocument(Service, Classes, prices('history=5,history-after=5')))
  const free = await fetch(`${r.url}/lookup`, post(ask('list')))
  assert.equal(free.status, 200)
  assert.deepEqual(await free.json(), { type: 'output-list', outputs: [], asked: 'list' })
  const bad = await fetch(`${r.url}/lookup`, post(ask('nosuch')))
  assert.equal(bad.status, 400)
  assert.deepEqual(await bad.json(), { error: 'no such question' })
  assert.equal((await fetch(`${r.url}/lookup`, { method: 'POST', body: 'nope' })).status, 400)
  assert.equal((await fetch(`${r.url}/lookup`, { method: 'POST', body: 'null' })).status, 400)
  assert.equal((await fetch(`${r.url}/nosuch`)).status, 404)
  const priced = await fetch(`${r.url}/lookup`, post(ask('history')))
  assert.equal(priced.status, 401, 'a priced class is asked over BRC-104')
  assert.equal(((await priced.json()) as { code: string }).code, 'ERR_AUTH_REQUIRED')
  assert.equal((await fetch(`${r.url}/lookup`, post(ask('list'), { 'x-bsv-auth-nonce': 'x' }))).status, 400, 'partial BRC-104 headers')
  assert.equal((await fetch(`${r.url}/lookup`, { method: 'POST', body: 'x'.repeat((64 << 10) + 1) })).status, 413)
  r.ls.restored = false
  assert.equal((await fetch(`${r.url}/lookup`, post(ask('list')))).status, 503)
  assert.equal((await fetch(`${r.url}/lookup`, post(ask('nosuch')))).status, 400, 'a question is classified before readiness is asked')

  const none = await rig({ prices: '' })
  assert.equal((await fetch(`${none.url}${termsPath(Service)}`)).status, 404, 'a host that prices nothing serves no terms')
  assert.equal((await fetch(`${none.url}/lookup`, post(ask('history')))).status, 200)
})

test('a price without a payee wallet, a receiver or headers is refused at construction', async () => {
  const { LookupFront } = await import('./front.js')
  const r = await rig()
  assert.throws(
    () => new LookupFront({ app: 'example', service: Service, classes: Classes, paymentProtocol: Protocol, ls: r.ls, host: r.host, prices: prices('history=5,history-after=5') }),
    { message: 'example: history has a price, which needs a payee wallet, a payment receiver and a header source' },
  )
})

test('over BRC-104: a free question is answered and signed; a priced one is answered 402, paid, and answered', async () => {
  const r = await rig()
  const af = new AuthFetch(r.wallet as unknown as WalletInterface)
  const free = await af.fetch(`${r.url}/lookup`, post(ask('list')))
  assert.equal(free.status, 200)
  assert.equal(free.headers.get('x-bsv-auth-identity-key'), payeeKey, 'signed by the payee')
  const paid = await quietly(async () => await af.fetch(`${r.url}/lookup`, post(ask('history'))))
  assert.equal(paid.status, 200)
  assert.equal(paid.headers.get('x-bsv-payment-satoshis-paid'), '5')
  assert.equal(r.receiver.payments.length, 1)
  const p = r.receiver.payments[0]!
  assert.equal(p.senderIdentityKey, r.client)
  assert.equal(p.class, 'history')
  assert.equal(p.satoshis, 5)
  assert.equal(p.decision, 'fast')
  const { publicKey } = await payeeWallet.getPublicKey({ protocolID: Protocol, keyID: `${p.derivationPrefix} ${p.derivationSuffix}`, counterparty: p.senderIdentityKey, forSelf: true })
  assert.equal(Transaction.fromAtomicBEEF(p.beef).outputs[0]!.lockingScript.toHex(), new P2PKH().lock(PublicKey.fromString(publicKey).toAddress()).toHex())
  assert.equal(r.host.count('example_payments_total', { decision: 'fast', reason: 'at-or-below-threshold' }), 1)
  assert.equal(r.host.count('example_payments_total', { decision: 'requested', reason: 'no-payment' }), 1)
})

test("a payment is refused unless the prefix is the server's, output 0 pays the price to the derived key, and it verifies; it buys one question", async () => {
  const r = await rig()
  const af = new AuthFetch(r.wallet as unknown as WalletInterface)
  const send = async (payment: string): Promise<{ status: number; code?: string }> => {
    const res = await af.fetch(`${r.url}/lookup`, post(ask('history'), { 'x-bsv-payment': payment }))
    const body = (await res.json()) as { code?: string }
    return { status: res.status, code: body.code }
  }
  const prefix = await createNonce(payeeWallet as unknown as WalletInterface)
  const suffix = Utils.toBase64(Random(32))
  const derived = async (counterparty: string): Promise<string> => {
    const { publicKey } = await r.wallet.getPublicKey({ protocolID: Protocol, keyID: `${prefix} ${suffix}`, counterparty })
    return new P2PKH().lock(PublicKey.fromString(publicKey).toAddress()).toHex()
  }
  const header = (tx: Transaction, pre = prefix): string => JSON.stringify({ derivationPrefix: pre, derivationSuffix: suffix, transaction: Utils.toBase64(tx.toAtomicBEEF()) })
  const good = await r.wallet.pay([{ satoshis: 5, lockingScript: await derived(payeeKey) }])

  assert.deepEqual(await send('not json'), { status: 400, code: 'ERR_MALFORMED_PAYMENT' })
  assert.deepEqual(await send(JSON.stringify({ derivationPrefix: prefix, derivationSuffix: suffix })), { status: 400, code: 'ERR_MALFORMED_PAYMENT' })
  assert.deepEqual(await send(JSON.stringify({ derivationPrefix: prefix, derivationSuffix: suffix, transaction: 'AA=' })), { status: 400, code: 'ERR_MALFORMED_PAYMENT' })
  assert.deepEqual(await send(header(good, Utils.toBase64(Random(32)))), { status: 400, code: 'ERR_INVALID_DERIVATION_PREFIX' })
  assert.deepEqual(await send(JSON.stringify({ derivationPrefix: prefix, derivationSuffix: suffix, transaction: 'AAAA' })), { status: 400, code: 'ERR_INVALID_PAYMENT' })
  assert.deepEqual(await send(header(await r.wallet.pay([{ satoshis: 4, lockingScript: await derived(payeeKey) }]))), { status: 400, code: 'ERR_INVALID_PAYMENT' })
  const other = PrivateKey.fromRandom().toPublicKey().toString()
  assert.deepEqual(await send(header(await r.wallet.pay([{ satoshis: 5, lockingScript: await derived(other) }]))), { status: 400, code: 'ERR_INVALID_PAYMENT' })
  const stray = fromNowhere([{ satoshis: 100000, lockingScript: new P2PKH().lock(r.wallet.key.toAddress()) }])
  new Chain(99000).mine(stray)
  assert.deepEqual(await send(header(await r.wallet.pay([{ satoshis: 5, lockingScript: await derived(payeeKey) }], stray))), { status: 400, code: 'ERR_PAYMENT_SPV' })
  assert.equal(r.receiver.payments.length, 0, 'nothing refused was taken')
  assert.equal(r.host.count('example_payments_total', { decision: 'refuse', reason: 'wrong-script' }), 1)
  assert.equal(r.host.count('example_payments_total', { decision: 'refuse', reason: 'spv-failed' }), 1)
  assert.equal((await send(header(good))).status, 200)
  assert.deepEqual(await send(header(good)), { status: 409, code: 'ERR_PAYMENT_REPLAYED' })
  assert.equal(r.receiver.payments.length, 1)
})

test('one coin pays once: conflicting payments that spend it buy one answer, not one each', async () => {
  const r = await rig()
  class OneCoin extends PayingWallet {
    readonly same = this.coin()
    override async createAction(args: CreateActionArgs): Promise<CreateActionResult> {
      const tx = await this.pay((args.outputs ?? []).map((o) => ({ satoshis: o.satoshis, lockingScript: o.lockingScript })), this.same)
      return { tx: tx.toAtomicBEEF(), txid: tx.id('hex') }
    }
  }
  const af = new AuthFetch(new OneCoin(r.wallet.key, r.chain) as unknown as WalletInterface)
  const statuses: number[] = []
  for (let i = 0; i < 3; i++) statuses.push((await quietly(async () => await af.fetch(`${r.url}/lookup`, post(ask('history'))))).status)
  assert.deepEqual(statuses, [200, 409, 409])
  assert.equal(r.receiver.payments.length, 1)
  assert.equal(r.host.count('example_payments_total', { decision: 'refuse', reason: 'conflict' }), 2)
})

const session = (nonce: string, key: string, lastUpdate = 0): PeerSession => ({ isAuthenticated: true, sessionNonce: nonce, peerIdentityKey: key, lastUpdate })

test('the BRC-104 session store is bounded: a cap that forgets the least recently used, and an idle life', () => {
  let now = 1_000
  const evicted: string[] = []
  const s = new BoundedSessions(2, 60_000, () => now, (why) => evicted.push(why))
  s.addSession(session('n1', 'k1'))
  s.addSession(session('n2', 'k2'))
  assert.equal(s.getSession('n1')?.sessionNonce, 'n1')
  s.addSession(session('n3', 'k3'))
  assert.equal(s.size, 2)
  assert.equal(s.hasSession('n2'), false, 'the least recently used went at the cap')
  assert.deepEqual(evicted, ['cap'])
  assert.equal(s.getSession('k3')?.sessionNonce, 'n3', 'looked up by identity key too')
  s.updateSession(session('n3', 'k3', 5))
  assert.equal(s.size, 2)
  now += 60_000
  assert.equal(s.getSession('n1'), undefined, 'idle for the whole life: forgotten')
  assert.equal(s.getSession('k3'), undefined)
  assert.equal(s.size, 0)
  assert.deepEqual(evicted, ['cap', 'idle', 'idle'])
  s.addSession(session('n4', 'k4'))
  s.removeSession(session('n4', 'k4'))
  assert.equal(s.hasSession('k4'), false)
  assert.throws(() => new BoundedSessions(0, 1), /max/)
  assert.throws(() => new BoundedSessions(1, 0), /ttlMs/)
  assert.throws(() => s.addSession({ isAuthenticated: false, lastUpdate: 0 }), /sessionNonce/)
})

test('the terms route keeps at most its bound of sessions; a client whose session went shakes hands again', async () => {
  const r = await rig({ sessions: { max: 2, ttlSeconds: 600 }, budget: { perSec: 100, burst: 100, perAddressPerSec: 100, addressBurst: 100 } })
  const clients = [0, 1, 2].map(() => new AuthFetch(new ProtoWallet(PrivateKey.fromRandom()) as unknown as WalletInterface))
  for (const c of clients) assert.equal((await c.fetch(`${r.url}/lookup`, post(ask('list')))).status, 200)
  assert.equal(r.front.sessions.size, 2)
  assert.equal(r.host.count('example_sessions_evicted_total', { why: 'cap' }), 1)
  assert.equal(r.host.gauges.get('example_sessions')?.(), 2)
  const res = await quietly(async () => await clients[0]!.fetch(`${r.url}/lookup`, post(ask('list'))))
  assert.equal(res.status, 200)
  assert.equal(r.host.count('example_sessions_evicted_total', { why: 'cap' }), 2)
})

/** One BRC-104 initial request, raw, from a fresh identity, sent from localAddress. */
async function shake(url: string, localAddress: string): Promise<{ status: number; retryAfter?: string }> {
  const body = JSON.stringify({
    version: '0.1',
    messageType: 'initialRequest',
    identityKey: PrivateKey.fromRandom().toPublicKey().toString(),
    initialNonce: Utils.toBase64(Random(32)),
    requestedCertificates: { certifiers: [], types: {} },
  })
  return await new Promise((resolve, reject) => {
    const req = request(`${url}/.well-known/auth`, { method: 'POST', localAddress, headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(body) } }, (res) => {
      res.resume()
      res.on('end', () => resolve({ status: res.statusCode ?? 0, retryAfter: res.headers['retry-after'] as string | undefined }))
    })
    req.on('error', reject)
    req.end(body)
  })
}

test('a handshake flood is refused 429 before any signature, per address and then for the route', async () => {
  const r = await rig({ budget: { perSec: 0.2, burst: 5, perAddressPerSec: 0.1, addressBurst: 2 } })
  const flood = await Promise.all(Array.from({ length: 30 }, () => shake(r.url, '127.0.0.2')))
  const limited = flood.filter((x) => x.status === 429)
  assert.equal(flood.filter((x) => x.status === 200).length, 2, 'the address burst')
  assert.equal(limited.length, 28)
  assert.ok(limited.every((x) => Number(x.retryAfter) >= 1), 'each refusal says when to come back')
  assert.equal(r.signatures(), 2, 'one signature per answered handshake, none for a refused one')
  assert.equal(r.host.count('example_handshakes_total', { result: 'accepted' }), 2)
  assert.equal(r.host.count('example_handshakes_total', { result: 'limited_address' }), 28)
  const others = await Promise.all(['127.0.0.3', '127.0.0.3', '127.0.0.4', '127.0.0.5'].map((a) => shake(r.url, a)))
  assert.deepEqual(others.map((x) => x.status).sort(), [200, 200, 200, 429])
  assert.equal(r.host.count('example_handshakes_total', { result: 'limited_global' }), 1)
})

test('a malformed handshake inside the budget is counted failed and costs no signature', async () => {
  const r = await rig()
  assert.equal((await fetch(`${r.url}/.well-known/auth`, post({ messageType: 'general' }))).status, 400)
  assert.equal((await fetch(`${r.url}/.well-known/auth`, { method: 'POST', body: 'nope' })).status, 400)
  assert.equal(r.host.count('example_handshakes_total', { result: 'failed' }), 2)
  assert.equal(r.signatures(), 0)
})

/** A server in front of the route that keeps each request as it arrived, to send it again. */
async function recording(r: Rig): Promise<{ url: string; port: number; requests: Array<{ method: string; url: string; headers: IncomingHttpHeaders; body: Buffer[] }> }> {
  const requests: Array<{ method: string; url: string; headers: IncomingHttpHeaders; body: Buffer[] }> = []
  const server = createServer((req, res) => {
    const body: Buffer[] = []
    requests.push({ method: req.method ?? 'GET', url: req.url ?? '/', headers: { ...req.headers }, body })
    req.on('data', (c: Buffer) => body.push(c))
    r.front.handler(req, res)
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  servers.push(server)
  const port = (server.address() as AddressInfo).port
  return { url: `http://127.0.0.1:${port}`, port, requests }
}

for (const placement of ['after-verify', 'before-verify'] as const) {
  test(`${placement}: a signed request sent again byte for byte is refused 401 before any signature`, async () => {
    const r = await rig({ responseBudget: placement })
    const rec = await recording(r)
    const af = new AuthFetch(r.wallet as unknown as WalletInterface)
    assert.equal((await af.fetch(`${rec.url}/lookup`, post(ask('list')))).status, 200)
    const sent = rec.requests.find((x) => x.url === '/lookup' && x.headers['x-bsv-auth-request-id'] !== undefined)!
    const before = r.signatures()
    const replay = async (): Promise<number> =>
      await new Promise((resolve, reject) => {
        const q = request({ host: '127.0.0.1', port: rec.port, path: sent.url, method: sent.method, headers: sent.headers }, (res) => {
          res.resume()
          res.on('end', () => resolve(res.statusCode ?? 0))
        })
        q.on('error', reject)
        q.end(Buffer.concat(sent.body))
      })
    const statuses = new Set<number>()
    for (let i = 0; i < 20; i++) statuses.add(await replay())
    for (const s of await Promise.all(Array.from({ length: 10 }, replay))) statuses.add(s)
    assert.deepEqual([...statuses], [401])
    assert.equal(r.signatures(), before, 'no signature for a replay')
    assert.equal(r.host.count('example_requests_total', { result: 'replayed' }), 30)
    assert.equal(r.host.count('example_requests_total', { result: 'limited' }), 0, 'a replay spends no budget')
    assert.equal((await af.fetch(`${rec.url}/lookup`, post(ask('list')))).status, 200, 'the session goes on')
  })

  test(`${placement}: a session has a budget of signed responses; over it a request is refused 429 and nothing is signed`, async () => {
    const r = await rig({ responses: { perSec: 0.5, burst: 3 }, responseBudget: placement })
    const af = new AuthFetch(r.wallet as unknown as WalletInterface)
    const one = async (): Promise<number> => {
      try {
        return (await quietly(async () => await af.fetch(`${r.url}/lookup`, post(ask('list'))))).status
      } catch {
        // The SDK's client raises on a response the server did not sign.
        return 429
      }
    }
    const statuses: number[] = []
    for (let i = 0; i < 5; i++) statuses.push(await one())
    assert.deepEqual(statuses, [200, 200, 200, 429, 429])
    assert.equal(r.signatures(), 4, 'the handshake and three responses')
    assert.equal(r.host.count('example_requests_total', { result: 'limited' }), 2)
    const other = new AuthFetch(new ProtoWallet(PrivateKey.fromRandom()) as unknown as WalletInterface)
    assert.equal((await other.fetch(`${r.url}/lookup`, post(ask('list')))).status, 200, 'another session has its own budget')
  })
}

/** Raw requests on one session with a forged signature: where the budget sits decides what they cost. */
test('a forged request spends the session budget only when it is taken before verification', async () => {
  for (const [placement, limited] of [
    ['after-verify', 0],
    ['before-verify', 2],
  ] as const) {
    const r = await rig({ responses: { perSec: 0.01, burst: 3 }, responseBudget: placement })
    const rec = await recording(r)
    const af = new AuthFetch(r.wallet as unknown as WalletInterface)
    assert.equal((await af.fetch(`${rec.url}/lookup`, post(ask('list')))).status, 200)
    const sent = rec.requests.find((x) => x.url === '/lookup' && x.headers['x-bsv-auth-request-id'] !== undefined)!
    const forged = async (): Promise<number> =>
      await new Promise((resolve, reject) => {
        const headers = { ...sent.headers, 'x-bsv-auth-request-id': Utils.toBase64(Random(32)) }
        const q = request({ host: '127.0.0.1', port: rec.port, path: sent.url, method: sent.method, headers }, (res) => {
          res.resume()
          res.on('end', () => resolve(res.statusCode ?? 0))
        })
        q.on('error', reject)
        q.end(Buffer.concat(sent.body))
      })
    const statuses: number[] = []
    for (let i = 0; i < 4; i++) statuses.push(await forged())
    assert.deepEqual(statuses, placement === 'after-verify' ? [401, 401, 401, 401] : [401, 401, 429, 429], placement)
    assert.equal(r.host.count('example_requests_total', { result: 'limited' }), limited, placement)
  }
})

/** Asks one priced question, paying as AuthFetch does; a held answer (402, no BRC-105 headers) throws in the SDK and is status 0. */
function asker(r: Rig, wallet: PayingWallet = r.wallet): () => Promise<number> {
  const af = new AuthFetch(wallet as unknown as WalletInterface)
  return async () => {
    try {
      return (await quietly(async () => await af.fetch(`${r.url}/lookup`, post(ask('history'))))).status
    } catch {
      return 0
    }
  }
}

const last = (r: Rig): ReceivedPayment => r.receiver.payments[r.receiver.payments.length - 1]!

test('acceptance: a small paid lookup is broadcast by the host, then answered; the watch releases it once mined', async () => {
  const r = await rig()
  assert.equal(await asker(r)(), 200)
  const p = last(r)
  assert.deepEqual(r.net.sent, [p.txid], 'broadcast before the answer')
  assert.deepEqual(r.gate.watching, [p.txid])
  r.net.mine(p.txid)
  assert.deepEqual((await r.gate.sweep()).map((e) => e.kind), ['confirmed'])
  assert.deepEqual(r.gate.watching, [])
  assert.equal(r.host.count('example_payment_events_total', { kind: 'confirmed' }), 1)
})

test('acceptance: a payment above the threshold is held 402 until it mines, then the same payment is answered once', async () => {
  const r = await rig({ policy: { thresholdSats: 4 } })
  assert.equal(await asker(r)(), 0)
  const p = last(r)
  assert.equal(p.decision, 'hold')
  assert.equal(p.reason, 'above-threshold')
  const header = JSON.stringify({ derivationPrefix: p.derivationPrefix, derivationSuffix: p.derivationSuffix, transaction: Utils.toBase64(p.beef) })
  const af = new AuthFetch(r.wallet as unknown as WalletInterface)
  const again = async (): Promise<number> => {
    try {
      return (await af.fetch(`${r.url}/lookup`, post(ask('history'), { 'x-bsv-payment': header }))).status
    } catch {
      return 0
    }
  }
  assert.equal(await again(), 0, 'still held before it mines')
  r.net.mine(p.txid)
  assert.equal(await again(), 200)
  assert.equal(last(r).decision, 'mined')
  assert.equal(await again(), 409, 'and answered once')
})

test('acceptance: refusals, a silent network, a failed broadcast, an unknown spend view, and a host with no network', async () => {
  const r = await rig()
  const one = asker(r)
  r.net.mode = 'reject'
  assert.equal(await one(), 400)
  r.net.mode = 'refuse-http'
  assert.equal(await one(), 400)
  assert.equal(r.host.count('example_payments_total', { decision: 'refuse', reason: 'network-refused' }), 2)
  assert.ok(r.host.logged.some((l) => l.msg === 'example payment refused'))
  r.net.mode = 'silent'
  assert.equal(await one(), 0)
  assert.equal(last(r).reason, 'no-network-verdict')
  r.net.mode = 'down'
  assert.equal(await one(), 0)
  assert.equal(last(r).reason, 'broadcast-unconfirmed')
  r.net.mode = 'accept'
  r.net.spendUnknown = true
  assert.equal(await one(), 0)
  assert.equal(last(r).reason, 'spend-view-unknown')
  assert.equal(r.gate.exposure.unmined(r.client).total, 0, 'every demotion released its charge')
  const off = await rig({ networked: false })
  assert.equal(await asker(off)(), 0)
  assert.equal(last(off).reason, 'no-broadcast-leg')
})

test('acceptance: arcade over HTTP: a refusal code refuses, "already known" is no verdict, "already spent" is never accepted', async () => {
  const replies: Array<[number, string]> = [
    [465, '{"title":"input already spent"}'],
    [409, '{"title":"txn-already-known"}'],
    [400, '{"title":"missing inputs or already spent"}'],
    [200, '{"txid":"x","txStatus":"SEEN_ON_NETWORK"}'],
  ]
  const server = createServer((req, res) => {
    req.resume()
    const [status, body] = replies.shift()!
    res.writeHead(status, { 'content-type': 'application/json' })
    res.end(body)
  })
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  servers.push(server)
  const a = new ArcadeHttp(`http://127.0.0.1:${(server.address() as AddressInfo).port}`)
  await assert.rejects(a.submit(Uint8Array.of(1)), BroadcastRefused)
  assert.deepEqual(await a.submit(Uint8Array.of(1)), { txStatus: 'RECEIVED', extraInfo: 'already known' })
  await assert.rejects(a.submit(Uint8Array.of(1)), (e: unknown) => !(e instanceof BroadcastRefused))
  assert.equal((await a.submit(Uint8Array.of(1))).txStatus, 'SEEN_ON_NETWORK')
})
