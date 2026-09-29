/**
 * The package's import rule. A runtime file imports only other runtime files
 * and '@bsv/sdk', and nothing from node:, so a browser reader can load the
 * runtime entry point. The testing entry point (src/testing/) may add node:
 * builtins and the runtime files. A test file may import anything under src/
 * and node: builtins. Nothing imports a file outside src/ or a package other
 * than '@bsv/sdk', and nothing that ships imports a test file.
 *
 * It reads the sources rather than the compiled tree: the sources are what
 * the package is built from, and a type-only import that tsc erases is still
 * an edge the package could not compile without.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

// dist/boundary.test.js -> ts/src/.
const root = new URL('../src/', import.meta.url)
const rootPath = fileURLToPath(root)

/**
 * Every module specifier a source names, whatever the statement: static
 * import and export-from (one line or many), a bare side-effect import, a
 * dynamic import and a require. A dynamic import or require of anything but
 * a string literal cannot be checked, so it is reported as `<computed>` and
 * refused. Text in a comment that looks like an import is refused too: the
 * scan errs towards failing.
 */
function specifiers(src: string): string[] {
  const found: string[] = []
  const patterns = [
    /\bfrom\s*['"]([^'"\n]+)['"]/g,
    /\bimport\s+['"]([^'"\n]+)['"]/g,
    /\bimport\s*\(\s*['"]([^'"\n]+)['"]\s*\)/g,
    /\brequire\s*\(\s*['"]([^'"\n]+)['"]\s*\)/g,
  ]
  for (const re of patterns) for (const m of src.matchAll(re)) found.push(m[1]!)
  for (const _ of src.matchAll(/\b(?:import|require)\s*\(\s*(?!['"][^'"\n]+['"]\s*\))/g)) found.push('<computed>')
  return found
}

function isTest(name: string): boolean {
  return name.endsWith('.test.ts')
}

/** A file of the testing entry point, which ships but only for tests. */
function isTesting(name: string): boolean {
  return name.startsWith('testing/') && !isTest(name)
}

/** Why spec may not be imported from file (relative to src/), or undefined when it may. */
function refusal(file: string, spec: string): string | undefined {
  if (spec === '@bsv/sdk') return undefined
  if (spec === '<computed>') return 'a specifier that is not a string literal'
  const runtime = !isTest(file) && !isTesting(file)
  if (spec.startsWith('node:')) return runtime ? 'a node: import in a runtime file' : undefined
  if (!spec.startsWith('./') && !spec.startsWith('../')) return 'a package other than @bsv/sdk'
  if (!spec.endsWith('.js')) return 'a relative import without its .js name'
  const target = fileURLToPath(new URL(spec.replace(/\.js$/, '.ts'), new URL(file, root)))
  if (!target.startsWith(rootPath)) return 'a file outside src/'
  if (!existsSync(target)) return 'a file that does not exist'
  const name = target.slice(rootPath.length).split('\\').join('/')
  if (isTest(name) && !isTest(file)) return 'a test file from a file that ships'
  if (isTesting(name) && runtime) return 'the testing entry point from a runtime file'
  return undefined
}

// This file is the one exception: its scanner fixtures are import
// statements by design. Its own imports are the four node: modules at its
// top.
const self = 'boundary.test.ts'

function sources(): string[] {
  return (readdirSync(root, { recursive: true }) as string[])
    .map((f) => f.split('\\').join('/'))
    .filter((f) => f.endsWith('.ts') && !f.endsWith('.d.ts') && f !== self)
    .sort()
}

test('the scanner finds every kind of import', () => {
  const src = [
    "import { a } from '@bsv/sdk'",
    "import type {\n  B,\n  C,\n} from './cbor.js'",
    "export { d } from '../record.js'",
    "export * from \"./store.js\"",
    "import 'node:fs'",
    "const m = await import('../keys.js')",
    "const n = await import(name)",
    "const r = require('node:path')",
  ].join('\n')
  assert.deepEqual(specifiers(src).sort(), ['../keys.js', '../record.js', './cbor.js', './store.js', '<computed>', '@bsv/sdk', 'node:fs', 'node:path'])
})

test('the rule refuses what it should', () => {
  assert.equal(refusal('carrier.ts', '@bsv/sdk'), undefined)
  assert.equal(refusal('carrier.ts', './fieldsig.js'), undefined)
  assert.equal(refusal('index.ts', './carrier.js'), undefined)
  assert.equal(refusal('carrier.test.ts', 'node:test'), undefined)
  assert.equal(refusal('carrier.test.ts', './carrier.js'), undefined)
  assert.equal(refusal('carrier.test.ts', './testing/index.js'), undefined)
  assert.equal(refusal('testing/index.ts', 'node:fs'), undefined)
  assert.equal(refusal('testing/index.ts', '../engine-types.js'), undefined)
  assert.equal(refusal('testing/testing.test.ts', './index.js'), undefined)
  assert.equal(refusal('carrier.ts', 'node:fs'), 'a node: import in a runtime file')
  assert.equal(refusal('carrier.ts', '@bsv/sdk/dist/esm/mod.js'), 'a package other than @bsv/sdk')
  assert.equal(refusal('carrier.ts', '@lightwebinc/bcommon'), 'a package other than @bsv/sdk')
  assert.equal(refusal('testing/index.ts', 'fs'), 'a package other than @bsv/sdk')
  assert.equal(refusal('carrier.ts', './fieldsig'), 'a relative import without its .js name')
  assert.equal(refusal('carrier.ts', '../record.js'), 'a file outside src/')
  assert.equal(refusal('carrier.test.ts', '../testdata/vectors.js'), 'a file outside src/')
  assert.equal(refusal('testing/index.ts', '../../record.js'), 'a file outside src/')
  assert.equal(refusal('carrier.ts', './nothing.js'), 'a file that does not exist')
  assert.equal(refusal('carrier.ts', './carrier.test.js'), 'a test file from a file that ships')
  assert.equal(refusal('testing/index.ts', '../carrier.test.js'), 'a test file from a file that ships')
  assert.equal(refusal('index.ts', './testing/index.js'), 'the testing entry point from a runtime file')
  assert.equal(refusal('carrier.ts', '<computed>'), 'a specifier that is not a string literal')
})

test('the package imports nothing outside src/ and @bsv/sdk, and runtime files nothing from node:', () => {
  const files = sources()
  // An empty scan would pass everything, so the files this rule exists for
  // must be among the ones read.
  for (const want of [
    'carrier.ts',
    'cbor.ts',
    'derive.ts',
    'engine-types.ts',
    'fieldsig.ts',
    'funding.ts',
    'index.ts',
    'store.ts',
    'testing/index.ts',
  ]) {
    assert.ok(files.includes(want), `${want} was not scanned`)
  }
  const bad: string[] = []
  let seen = 0
  for (const file of files) {
    for (const spec of specifiers(readFileSync(new URL(file, root), 'utf8'))) {
      seen++
      const why = refusal(file, spec)
      if (why !== undefined) bad.push(`${file} imports ${spec}: ${why}`)
    }
  }
  assert.ok(seen > 0, 'no import was found at all')
  assert.deepEqual(bad, [])
})

// The import rule keeps third-party code out of what the package's files
// load; the manifest is what keeps it out of what an install brings. A
// dependency would be installed beside the package, with a licence
// obligation NOTICE does not state, and a second @bsv/sdk would be a second
// copy of every type the host's engine checks with instanceof.
test('the package installs nothing: @bsv/sdk is a peer, at the version its tests run on', () => {
  const pkg = JSON.parse(readFileSync(new URL('../package.json', root), 'utf8')) as Record<string, unknown>
  for (const field of ['dependencies', 'optionalDependencies', 'bundleDependencies', 'bundledDependencies']) {
    assert.equal(pkg[field], undefined, `package.json declares ${field}`)
  }
  assert.deepEqual(pkg.peerDependencies, { '@bsv/sdk': '2.7.1' })
  const dev = pkg.devDependencies as Record<string, string>
  assert.equal(dev['@bsv/sdk'], '2.7.1', 'the tests must run on the peer version exactly')
  const sdk = JSON.parse(readFileSync(new URL('../node_modules/@bsv/sdk/package.json', root), 'utf8')) as { version: string }
  assert.equal(sdk.version, '2.7.1', 'the installed @bsv/sdk is not the peer version')
})

// The exports map names compiled files, and a typo there is invisible until
// a consumer's import fails. The tests run on the compiled tree, so every
// target must exist beside them, and each must be the compiled form of a
// source this file scans.
test('every entry point in the exports map is built', () => {
  const pkg = JSON.parse(readFileSync(new URL('../package.json', root), 'utf8')) as {
    exports: Record<string, { types: string; default: string }>
  }
  assert.deepEqual(Object.keys(pkg.exports).sort(), ['.', './testing'])
  const files = sources()
  for (const [entry, { types, default: js }] of Object.entries(pkg.exports)) {
    for (const target of [types, js]) {
      assert.ok(existsSync(new URL(`../${target}`, root)), `${entry}: ${target} was not built`)
    }
    const source = js.replace(/^\.\/dist\//, '').replace(/\.js$/, '.ts')
    assert.equal(types, js.replace(/\.js$/, '.d.ts'), `${entry}: types and runtime name different files`)
    assert.ok(files.includes(source), `${entry}: ${js} is not built from a scanned source`)
  }
})
