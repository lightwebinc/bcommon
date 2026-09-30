/**
 * A funding output: PushDrop `[tag]` with no signature under the producer's
 * derivation, so the script is `<33-byte key> OP_CHECKSIG <tag> OP_DROP` and
 * is spent by the same single signature a bare pay-to-public-key takes.
 * The twin of the Go package carrier's DecodeFunding.
 *
 * The tag is the application's, frozen at its first mint. It is what lets a
 * host admit the application's funding outputs into its topic without
 * admitting every pay-to-public-key a host sees, and admitting them is what
 * makes a later spend of one (a kill switch) visible: a funding output the
 * engine holds is one whose second spend the engine reports.
 */
import { OP, type PublicKey, type Script } from '@bsv/sdk'
import { decodeStrictPushDrop } from './pushdrop.js'

function sameBytes(a: readonly number[], b: readonly number[]): boolean {
  return a.length === b.length && a.every((x, i) => x === b[i])
}

/**
 * The locking key when the script is exactly a funding output under tag,
 * else undefined.
 *
 * PushDrop.decode is lenient: with lock-before it reads chunk 0 as the key
 * and collects fields from chunk 2 without checking that chunk 1 is
 * OP_CHECKSIG or that a DROP follows, so a bare pay-to-public-key with a
 * trailing push would decode with one field too. The chunk shape is checked
 * first so that only the exact script a producer writes is a funding output;
 * the decode then supplies the key and the field, and only in the one
 * encoding the template writes (decodeStrictPushDrop).
 */
export function decodeFunding(script: Script, tag: readonly number[]): PublicKey | undefined {
  const chunks = script.chunks
  if (chunks.length !== 4) return undefined
  const [key, checksig, tagChunk, drop] = chunks
  if (key?.data === undefined || key.data.length !== 33) return undefined
  if (checksig?.op !== OP.OP_CHECKSIG || drop?.op !== OP.OP_DROP) return undefined
  if (tagChunk?.data === undefined || !sameBytes(tagChunk.data, tag)) return undefined
  const decoded = decodeStrictPushDrop(script)
  if (decoded === undefined || decoded.fields.length !== 1) return undefined
  return decoded.lockingPublicKey
}
