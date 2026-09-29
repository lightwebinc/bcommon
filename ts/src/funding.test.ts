/**
 * decodeFunding under a neutral tag: the script a producer's wallet writes
 * (PushDrop [tag], no signature) decodes to its locking key, and the near
 * misses PushDrop.decode alone would accept do not.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { OP, PrivateKey, ProtoWallet, PushDrop, Script, type WalletInterface, type WalletProtocol } from '@bsv/sdk'
import { readerLockingKey } from './derive.js'
import { decodeFunding } from './funding.js'

// TEST-ONLY key.
const priv = PrivateKey.fromHex('42'.repeat(32))
const protocol: WalletProtocol = [2, 'example records']
const keyID = 'item'
const tag: readonly number[] = [0x78, 0x79, 0x02]

test('the script a wallet writes decodes to the reader key, under its tag only', async () => {
  // ProtoWallet has the two calls PushDrop.lock makes, not the action surface.
  const wallet = new ProtoWallet(priv) as unknown as WalletInterface
  const lock = await new PushDrop(wallet).lock([[...tag]], protocol, keyID, 'anyone', true, false, 'before')
  const want = readerLockingKey(protocol, keyID, priv.toPublicKey().toString()).toString()
  assert.equal(decodeFunding(lock, tag)?.toString(), want)
  assert.equal(decodeFunding(lock, [0x78, 0x79, 0x03]), undefined, 'another version')
  assert.equal(decodeFunding(lock, [0x78, 0x79]), undefined, 'a prefix of the tag')
  assert.equal(decodeFunding(lock, [...tag, 0x00]), undefined, 'the tag and one more byte')
})

test('the near misses are not funding outputs', () => {
  const key = Array.from(priv.toPublicKey().encode(true) as number[])
  const good = new Script([{ op: key.length, data: key }, { op: OP.OP_CHECKSIG }, { op: tag.length, data: [...tag] }, { op: OP.OP_DROP }])
  assert.equal(decodeFunding(good, tag)?.toString(), priv.toPublicKey().toString(), 'the constructed shape')

  const miss = (name: string, chunks: Script['chunks']): void => {
    assert.equal(decodeFunding(new Script(chunks), tag), undefined, name)
  }
  const [k, checksig, t, drop] = good.chunks as [Script['chunks'][0], Script['chunks'][0], Script['chunks'][0], Script['chunks'][0]]
  miss('bare P2PK', [k, checksig])
  miss('OP_2DROP', [k, checksig, t, { op: OP.OP_2DROP }])
  miss('OP_CHECKSIGVERIFY', [k, { op: OP.OP_CHECKSIGVERIFY }, t, drop])
  miss('trailing chunk', [k, checksig, t, drop, { op: OP.OP_TRUE }])
  miss('short key', [{ op: 32, data: key.slice(1) }, checksig, t, drop])
  miss('two fields', [k, checksig, t, t, { op: OP.OP_2DROP }])
  miss('empty', [])
})
