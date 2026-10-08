/**
 * WhatsOnChain as a host's node view, against its answers about one
 * mainnet transaction in block 900000, captured 2026-10-07: the spent
 * endpoint's 200, 404 (unspent) and 400 (unknown, never unspent), the
 * BEEF's proof, the TSC fallback, and the configuration that picks it.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { MerklePath } from '@bsv/sdk'
import { WocHttp, parseAcceptConfig, tscMerklePath, type TscProof } from './accept.js'

const source = (name: string): string => readFileSync(new URL(`../../../testdata/sources/${name}`, import.meta.url), 'utf8')

const tx = '52854619b1c2e78d6c1e9a91fdb14c4bef1b8d1897de3253a538e152bd055be6'
const parent = '4965bef51816db722c4c76b91dd753a2e5eef8abe6a846d888f2b5432aae86ae'
const block = '000000000000000002feb6a36e1b8bf81409d0252e285449e3d0ef2388c5506a'
const root = '62272ce3662923219acd98587fdb5c0b01557597036d8635207bda8a3fa72a7e'
const unknown = '0000000000000000000000000000000000000000000000000000000000000001'

async function serve(routes: Map<string, [number, string]>): Promise<{ server: Server; base: string; seen: string[] }> {
  const seen: string[] = []
  const server = createServer((req, res) => {
    seen.push(`${req.url ?? ''} ${String(req.headers.authorization ?? '')}`)
    const [status, body] = routes.get(req.url ?? '') ?? [404, 'Not Found']
    res.statusCode = status
    res.end(body)
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  return { server, base: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, seen }
}

function realRoutes(): Map<string, [number, string]> {
  return new Map<string, [number, string]>([
    [`/tx/${tx}/beef`, [200, source('woc-main-tx-beef.txt')]],
    [`/tx/${tx}/proof/tsc`, [200, source('woc-main-tx-tsc.json')]],
    [`/block/${block}/header`, [200, source('woc-main-block-hash-header.json')]],
    [`/tx/${parent}/4/spent`, [200, source('woc-main-spent-confirmed.json')]],
    [`/tx/${tx}/1/spent`, [400, 'Bad Request']],
    [`/tx/${unknown}/beef`, [500, source('woc-main-beef-unknown.txt')]],
  ])
}

test('spender: 200 names it, 404 is unspent, 400 is never unspent', async () => {
  const { server, base, seen } = await serve(realRoutes())
  try {
    const w = new WocHttp('main', 'mainnet_key', base, 1000)
    assert.equal(await w.spender(parent, 4), tx)
    assert.equal(await w.spender(tx, 0), '')
    await assert.rejects(w.spender(tx, 1), /does not know/)
    await assert.rejects(w.spender('zz', 0), /not an outpoint/)
    assert.ok(seen.every((s) => s.endsWith(' mainnet_key')))
  } finally {
    server.close()
  }
})

test('proof: from the BEEF, or the TSC fallback, rooted at the real block', async () => {
  const routes = realRoutes()
  const { server, base } = await serve(routes)
  try {
    const w = new WocHttp('main', undefined, base, 1000)
    const mp = await w.proof(tx)
    assert.ok(mp instanceof MerklePath)
    assert.equal(mp.blockHeight, 900000)
    assert.equal(mp.computeRoot(tx), root)
    assert.equal(await w.proof(unknown), undefined)
    routes.set(`/tx/${tx}/beef`, [502, 'bad gateway'])
    const viaTsc = await w.proof(tx)
    assert.equal(viaTsc?.blockHeight, 900000)
    assert.equal(viaTsc?.computeRoot(tx), root)
    routes.set(`/tx/${tx}/proof/tsc`, [200, 'null'])
    assert.equal(await w.proof(tx), undefined)
    routes.set(`/tx/${tx}/proof/tsc`, [500, 'down'])
    await assert.rejects(w.proof(tx), /tsc: 500/)
  } finally {
    server.close()
  }
})

test('a TSC path with a duplicate converts to the block root', () => {
  const p = JSON.parse(source('woc-main-tx-tsc.json')) as TscProof[]
  assert.equal(tscMerklePath(p[0] as TscProof, tx, 900000).computeRoot(tx), root)
  assert.throws(() => tscMerklePath(p[0] as TscProof, parent, 900000), /not/)
  assert.throws(() => tscMerklePath({ index: 4, txOrId: tx, target: block, nodes: ['*', '*'] }, tx, 1), /does not fit/)
  // Three transactions, the last proven: its level-0 sibling is itself.
  const dup = tscMerklePath({ index: 2, txOrId: tx, target: block, nodes: ['*', parent] }, tx, 7)
  assert.equal(dup.path[0]?.length, 2)
  assert.equal(dup.path[0]?.[1]?.duplicate, true)
})

test('configuration: <P>_CHAIN picks WhatsOnChain, never beside an asset URL', () => {
  const c = parseAcceptConfig('APP', { APP_CHAIN: 'woc:test', APP_WOC_KEY: 'k' })
  assert.equal(c.chain, 'test')
  assert.equal(c.wocKey, 'k')
  assert.equal(parseAcceptConfig('APP', {}).chain, undefined)
  assert.throws(() => parseAcceptConfig('APP', { APP_CHAIN: 'woc:regtest' }), /woc:main or woc:test/)
  assert.throws(() => parseAcceptConfig('APP', { APP_CHAIN: 'woc:main', APP_ASSET_URL: 'http://node' }), /keep one/)
  assert.throws(() => new WocHttp('regtest' as 'main'), /main or test/)
})

test('raw: the transaction WhatsOnChain answers as hex, held to the txid asked; another transaction or a 404 throws; the main entry exports it', async () => {
  const { Transaction: Tx, LockingScript } = await import('@bsv/sdk')
  const main = await import('../index.js')
  assert.equal(main.WocHttp, WocHttp, 'the browser-safe entry exports the same class')
  const mk = (n: number) => new Tx(1, [{ sourceTXID: '11'.repeat(32), sourceOutputIndex: n, unlockingScript: LockingScript.fromHex('51') as never, sequence: 0xffffffff }], [{ satoshis: 1, lockingScript: LockingScript.fromHex('51') }], 0)
  const a = mk(0)
  const b = mk(1)
  const { server, base } = await serve(
    new Map([
      [`/tx/${a.id('hex')}/hex`, [200, `${a.toHex()}\n`]],
      [`/tx/${b.id('hex')}/hex`, [200, a.toHex()]],
    ]),
  )
  try {
    const w = new WocHttp('main', undefined, base, 1000)
    assert.deepEqual(Array.from(await w.raw(a.id('hex'))), a.toBinary())
    await assert.rejects(w.raw(b.id('hex')), /asked for transaction/)
    await assert.rejects(w.raw(unknown), /does not hold/)
    await assert.rejects(w.raw('xyz'), /not a txid/)
  } finally {
    server.close()
  }
})
