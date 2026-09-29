/**
 * The reader's derivation on a neutral triple: what readerLockingKey returns
 * is the key the identity's own wallet locks to (counterparty anyone,
 * forSelf=true), the forSelf=false control is a different key, and both the
 * protocol and the key id are hashed in.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { KeyDeriver, PrivateKey, type WalletProtocol } from '@bsv/sdk'
import { readerLockingKey } from './derive.js'

// TEST-ONLY key.
const priv = PrivateKey.fromHex('42'.repeat(32))
const identityHex = priv.toPublicKey().toString()
const protocol: WalletProtocol = [2, 'example records']
const keyID = 'item'

test('the reader recomputes the key the identity locks to', () => {
  const own = new KeyDeriver(priv)
  const locked = own.derivePublicKey(protocol, keyID, 'anyone', true).toString()
  assert.equal(readerLockingKey(protocol, keyID, identityHex).toString(), locked)
  // forSelf=false is the anyone key's child, which no reader can tie to the
  // identity: the control that shows the row above is not a coincidence.
  assert.notEqual(own.derivePublicKey(protocol, keyID, 'anyone', false).toString(), locked)
  assert.notEqual(locked, identityHex, 'a derived key is never the identity key')
})

test('the protocol, the key id and the identity each change the key', () => {
  const base = readerLockingKey(protocol, keyID, identityHex).toString()
  assert.equal(readerLockingKey(protocol, keyID, identityHex).toString(), base, 'deterministic')
  assert.notEqual(readerLockingKey([2, 'other records'], keyID, identityHex).toString(), base)
  assert.notEqual(readerLockingKey([1, 'example records'], keyID, identityHex).toString(), base)
  assert.notEqual(readerLockingKey(protocol, 'other', identityHex).toString(), base)
  const other = PrivateKey.fromHex('43'.repeat(32)).toPublicKey().toString()
  assert.notEqual(readerLockingKey(protocol, keyID, other).toString(), base)
})

test('an identity that does not parse throws', () => {
  assert.throws(() => readerLockingKey(protocol, keyID, ''))
  assert.throws(() => readerLockingKey(protocol, keyID, 'zz'))
})
