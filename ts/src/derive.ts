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

const anyone = new KeyDeriver('anyone')

/**
 * Throws when identityHex does not parse as a public key. The SDK's parser
 * is lenient about the point itself, so callers treat a throw as one refusal
 * among others rather than as the whole of key validation.
 */
export function readerLockingKey(protocol: WalletProtocol, keyID: string, identityHex: string): PublicKey {
  return anyone.derivePublicKey(protocol, keyID, identityHex, false)
}
