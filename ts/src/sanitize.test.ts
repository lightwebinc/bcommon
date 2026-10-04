/**
 * The renderer filter against the corpus the Go sanitize package's tests
 * read, testdata/vectors/sanitize-v1.json: every case must come out as the
 * same bytes in both languages. The corpus is pinned to the table by its
 * SHA-256, and the table module must hold exactly the bytes the Go package
 * embeds.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { UnicodeVersion, filterBytes, filterText } from './sanitize.js'
import { unicodeTableJSON } from './sanitize-table.js'
import { fromHex, toHex } from './testing/index.js'

interface Corpus {
  unicode: string
  tableSha256: string
  cases: Array<{ name: string; inputHex: string; outputHex: string }>
}

// dist/sanitize.test.js -> ts/ -> the repository root.
const root = new URL('../../', import.meta.url)
const corpus = JSON.parse(readFileSync(new URL('testdata/vectors/sanitize-v1.json', root), 'utf8')) as Corpus
const utf8 = new TextEncoder()
const strict = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true })

test('the table is the Go package table, and the corpus is pinned to it', () => {
  assert.equal(unicodeTableJSON, readFileSync(new URL('sanitize/unicode-15.1.json', root), 'utf8'))
  assert.equal(corpus.unicode, UnicodeVersion)
  assert.equal(createHash('sha256').update(unicodeTableJSON, 'utf8').digest('hex'), corpus.tableSha256)
})

test('every corpus case gives the Go bytes', () => {
  assert.ok(corpus.cases.length >= 80, `${corpus.cases.length} cases`)
  for (const c of corpus.cases) {
    const input = fromHex(c.inputHex)
    const got = filterBytes(input)
    assert.equal(toHex(utf8.encode(got)), c.outputHex, c.name)
    assert.equal(filterText(got), got, `${c.name}: not a fixed point`)
    let text: string | undefined
    try {
      text = strict.decode(input)
    } catch {
      text = undefined
    }
    if (text !== undefined) assert.equal(filterText(text), got, `${c.name}: as a string`)
  }
})

test('a lone surrogate in a string is U+FFFD', () => {
  assert.equal(filterText('a\ud800b\udc00c'), 'a\ufffdb\ufffdc')
  assert.equal(filterText('\ud83d'), '\ufffd')
})
