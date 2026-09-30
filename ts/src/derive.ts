/**
 * The reader's side of a BRC-42/43 derivation.
 *
 * From the anyone root, the identity's key under the protocol and key id,
 * forSelf=false. It equals what the identity's own wallet produces with
 * counterparty anyone and forSelf=true, which is the only setting under which
 * a reader holding just the identity key can recompute the locking key AND
 * the embedded signature verifies under it. The Go twin,
 * pushdrop.Derivation.ExpectedLockingKey, makes the same call.
 *
 * The protocol and key id are the application's. Both are hashed into every
 * derived key, so an application freezes them at its first mint: changing
 * either orphans every object it ever locked.
 */
import { KeyDeriver, type PublicKey, type WalletProtocol } from '@bsv/sdk'
import { strictPublicKeyHex } from './pubkey.js'

const anyone = new KeyDeriver('anyone')

/**
 * Throws when identityHex is not the canonical encoding of a public key
 * (strictPublicKeyHex): 66 hex digits, the prefix 02 or 03, and an
 * x below the field prime on the curve. The SDK would take an alias such as
 * 02 || p+1 and derive from the point it names, so the check is made here,
 * as the Go carrier.Validate makes it on the identity key.
 */
export function readerLockingKey(protocol: WalletProtocol, keyID: string, identityHex: string): PublicKey {
  if (strictPublicKeyHex(identityHex) === undefined) throw new Error('identity key is not a canonical compressed public key')
  return anyone.derivePublicKey(protocol, keyID, identityHex, false)
}
