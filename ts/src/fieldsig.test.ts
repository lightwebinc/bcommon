/**
 * The field signature on a neutral triple: what a wallet signs over the
 * fields, counterparty anyone, verifies under the key a reader derives, and
 * anything malformed is false rather than a throw.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, ProtoWallet, type WalletProtocol } from '@bsv/sdk'
import { readerLockingKey } from './derive.js'
import { verifyFieldSignature } from './fieldsig.js'

// TEST-ONLY key.
const priv = PrivateKey.fromHex('42'.repeat(32))
const protocol: WalletProtocol = [2, 'example records']
const keyID = 'item'

test('a wallet field signature verifies under the reader key', async () => {
  const signed = [0x78, 0x79, 0x01, ...Array.from({ length: 32 }, (_, i) => i)]
  const { signature } = await new ProtoWallet(priv).createSignature({ data: signed, protocolID: protocol, keyID, counterparty: 'anyone' })
  const key = readerLockingKey(protocol, keyID, priv.toPublicKey().toString())
  assert.equal(verifyFieldSignature(key, signed, signature), true)

  const otherData = [...signed]
  otherData[0] = 0x79
  assert.equal(verifyFieldSignature(key, otherData, signature), false, 'other bytes')
  const otherKey = readerLockingKey(protocol, 'other', priv.toPublicKey().toString())
  assert.equal(verifyFieldSignature(otherKey, signed, signature), false, 'another key')
  const flipped = [...signature]
  flipped[10] = (flipped[10] ?? 0) ^ 0x01
  assert.equal(verifyFieldSignature(key, signed, flipped), false, 'a flipped signature byte')
})

test('a signature that does not parse is false, not a throw', () => {
  const key = priv.toPublicKey()
  assert.equal(verifyFieldSignature(key, [1, 2, 3], []), false)
  assert.equal(verifyFieldSignature(key, [1, 2, 3], [0x30, 0x02, 0x01]), false)
})
