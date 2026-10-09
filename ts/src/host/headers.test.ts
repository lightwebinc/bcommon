import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createServer, type Server } from 'node:http'
import type { AddressInfo } from 'node:net'
import { HeaderTracker, isNativeSource, MaxHeaderAnswer } from './headers.js'

/** A header source answering roots by height and a tip, counting requests. */
async function source(answer: (path: string) => { status: number; body: string; location?: string }): Promise<{ url: string; server: Server; asked: string[] }> {
  const asked: string[] = []
  const server = createServer((req, res) => {
    asked.push(req.url ?? '')
    const a = answer(req.url ?? '')
    res.writeHead(a.status, a.location === undefined ? { 'content-type': 'application/json' } : { location: a.location })
    res.end(a.body)
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  return { url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`, server, asked }
}

const root = 'ab'.repeat(32)

test('roots: a confirmed root is valid and reused, a 404 is not valid, other statuses are errors', async () => {
  const s = await source((p) =>
    p === '/v1/root/5' ? { status: 200, body: JSON.stringify({ merkleRoot: root }) } : p === '/v1/root/6' ? { status: 404, body: '{}' } : { status: 500, body: 'no' },
  )
  try {
    let now = 0
    const h = new HeaderTracker(s.url + '/', 2000, () => now)
    assert.equal(await h.isValidRootForHeight(root, 5), true)
    assert.equal(await h.isValidRootForHeight(root, 5), true)
    assert.equal(s.asked.length, 1, 'a proven root is reused')
    now = 11 * 60_000
    assert.equal(await h.isValidRootForHeight(root, 5), true)
    assert.equal(s.asked.length, 2, 'and asked again after ten minutes')
    assert.equal(await h.isValidRootForHeight('cd'.repeat(32), 5), false)
    assert.equal(await h.isValidRootForHeight(root, 6), false)
    await assert.rejects(h.isValidRootForHeight(root, 7), /status 500/)
  } finally {
    s.server.close()
  }
})

test('the tip, an answer over the bound, a redirect and an answer that is not JSON', async () => {
  const s = await source((p) => {
    if (p === '/v1/tip') return { status: 200, body: JSON.stringify({ height: 42 }) }
    if (p === '/v1/root/1') return { status: 200, body: 'x'.repeat(MaxHeaderAnswer + 1) }
    if (p === '/v1/root/2') return { status: 302, body: '', location: '/v1/root/5' }
    return { status: 200, body: 'not json' }
  })
  try {
    const h = new HeaderTracker(s.url)
    assert.equal(await h.currentHeight(), 42)
    await assert.rejects(h.isValidRootForHeight(root, 1), /over 65536 bytes/)
    await assert.rejects(h.isValidRootForHeight(root, 2))
    await assert.rejects(h.isValidRootForHeight(root, 3), /not JSON/)
  } finally {
    s.server.close()
  }
})

test('a source is an http or https URL', () => {
  assert.throws(() => new HeaderTracker('ftp://x'), /not an http or https URL/)
  assert.throws(() => new HeaderTracker('nonsense'), /not a URL/)
  assert.equal(isNativeSource('https://headers.example'), true)
  assert.equal(isNativeSource('woc:main'), false)
  assert.equal(isNativeSource('http://'), false)
})
