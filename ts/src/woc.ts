/**
 * WhatsOnChain as a node view, with no node: who spent an output, a
 * transaction's proof, and its raw bytes. It imports nothing from node:,
 * so a browser page can ask it (WhatsOnChain answers CORS for any origin)
 * as a host does; "@lightwebinc/bcommon/host" re-exports it. The twin of
 * Go's nodeapi.WoC: an unknown output is an error and never "unspent";
 * "unspent" is WhatsOnChain's word and is trusted as such.
 */
import { MerklePath, Transaction } from '@bsv/sdk'

const isTxid = (s: unknown): s is string => typeof s === 'string' && /^[0-9a-fA-F]{64}$/.test(s)
const timeout = 10_000
const sleep = (ms: number): Promise<void> => new Promise((r) => (setTimeout(r, ms) as { unref?: () => void }).unref?.())
/** The largest transaction raw() reads (bytes). */
export const MaxTxBytes = 8 << 20

/** The node's view: who spent an output, and a transaction's proof. */
export interface NodeView {
  /** '' when the node holds the output unspent, the spender's txid when spent; throws on anything else. */
  spender(txid: string, vout: number): Promise<string>
  /** The proof of txid, or undefined while it is not mined. */
  proof(txid: string): Promise<MerklePath | undefined>
}

/** WhatsOnChain's free tier: "Up to 3 requests/sec". */
export const WocFreeRate = 3

/** One entry of WhatsOnChain's /tx/{txid}/proof/tsc answer. */
export interface TscProof {
  index: number
  txOrId: string
  target: string
  nodes: string[]
}

/** A TSC merkle proof of txid as a BRC-74 path at height; throws on a proof of something else. */
export function tscMerklePath(p: TscProof, txid: string, height: number): MerklePath {
  if (typeof p.txOrId !== 'string' || p.txOrId.toLowerCase() !== txid.toLowerCase()) throw new Error(`the tsc proof is for ${String(p.txOrId)}, not ${txid}`)
  if (!Array.isArray(p.nodes) || p.nodes.length > 64 || !Number.isSafeInteger(p.index) || p.index < 0) throw new Error('the tsc proof is malformed')
  if (p.nodes.length < 53 && p.index >= 2 ** p.nodes.length) throw new Error(`tsc index ${p.index} does not fit ${p.nodes.length} levels`)
  const path: Array<Array<{ offset: number; hash?: string; txid?: boolean; duplicate?: boolean }>> = [[{ offset: p.index, hash: txid.toLowerCase(), txid: true }]]
  p.nodes.forEach((n, level) => {
    const offset = Math.floor(p.index / 2 ** level) ^ 1
    if (n !== '*' && !isTxid(n)) throw new Error(`tsc node ${level} is not a hash`)
    const el = n === '*' ? { offset, duplicate: true } : { offset, hash: n.toLowerCase() }
    path[level] = [...(path[level] ?? []), el].sort((a, b) => a.offset - b.offset)
  })
  return new MerklePath(height, path)
}

/**
 * WhatsOnChain as the node view, for a host with no node: who spent an
 * output, from GET /tx/{txid}/{vout}/spent, where a 404 is WhatsOnChain's
 * "known, not spent" and a 400 its "unknown" (also its answer for an
 * unspendable output), which throws and is never read as unspent; and a
 * proof, from the undocumented GET /tx/{txid}/beef with the documented
 * /proof/tsc as the fallback. The gate checks every proof against the
 * host's headers. Requests are paced to rate a second (default the free
 * tier's 3); a key is sent as the Authorization header.
 */
export class WocHttp implements NodeView {
  private next = 0
  private readonly base: string
  constructor(
    network: 'main' | 'test',
    private readonly key?: string,
    base?: string,
    private readonly rate = WocFreeRate,
  ) {
    if (network !== 'main' && network !== 'test') throw new Error(`WhatsOnChain serves main or test, not ${String(network)}`)
    this.base = (base ?? `https://api.whatsonchain.com/v1/bsv/${network}`).replace(/\/+$/, '')
  }

  private async get(p: string): Promise<Response> {
    const gap = 1000 / (this.rate > 0 ? this.rate : WocFreeRate)
    for (let attempt = 0; ; attempt++) {
      const now = Date.now()
      const slot = Math.max(now, this.next)
      this.next = slot + gap
      if (slot > now) await sleep(slot - now)
      const headers: Record<string, string> = {}
      if (this.key !== undefined && this.key !== '') headers.authorization = this.key
      const res = await fetch(`${this.base}${p}`, { headers, signal: AbortSignal.timeout(timeout) })
      if (res.status === 429 && attempt < 3) {
        await sleep([200, 1000, 3000][attempt] ?? 3000)
        continue
      }
      return res
    }
  }

  async spender(txid: string, vout: number): Promise<string> {
    if (!isTxid(txid) || !Number.isSafeInteger(vout) || vout < 0) throw new Error('not an outpoint')
    const res = await this.get(`/tx/${txid}/${vout}/spent`)
    if (res.status === 404) return ''
    if (res.status === 400) throw new Error(`WhatsOnChain does not know ${txid}.${vout}`)
    if (res.status !== 200) throw new Error(`WhatsOnChain answers ${res.status} for ${txid}.${vout}`)
    const s = (await res.json()) as { txid?: unknown }
    if (!isTxid(s?.txid)) throw new Error('WhatsOnChain names a spender that is not a txid')
    return s.txid.toLowerCase()
  }

  /** GET /tx/{txid}/hex: the raw transaction, which must hash to txid (Go nodeapi.WoC.TxRaw); a 404 throws. */
  async raw(txid: string): Promise<Uint8Array> {
    if (!isTxid(txid)) throw new Error('not a txid')
    const res = await this.get(`/tx/${txid}/hex`)
    if (res.status === 404) throw new Error(`WhatsOnChain does not hold transaction ${txid}`)
    if (res.status !== 200) throw new Error(`WhatsOnChain answers ${res.status} for transaction ${txid}`)
    const text = (await res.text()).trim()
    if (text.length > 2 * MaxTxBytes || !/^([0-9a-fA-F]{2})*$/.test(text)) throw new Error(`transaction ${txid}: the answer is not hex within its bound`)
    const tx = Transaction.fromHex(text)
    if (tx.id('hex') !== txid.toLowerCase()) throw new Error(`asked for transaction ${txid}, answered ${tx.id('hex')}`)
    return Uint8Array.from(tx.toBinary())
  }

  async proof(txid: string): Promise<MerklePath | undefined> {
    if (!isTxid(txid)) throw new Error('not a txid')
    let failed: string
    const res = await this.get(`/tx/${txid}/beef`)
    const body = await res.text()
    if (res.status === 422 || res.status === 404 || (res.status === 500 && body.includes('No such mempool or blockchain transaction'))) return undefined
    if (res.status === 200) {
      try {
        const tx = Transaction.fromHexBEEF(body.trim())
        if (tx.id('hex') !== txid.toLowerCase()) throw new Error(`the BEEF is transaction ${tx.id('hex')}`)
        if (tx.merklePath === undefined) return undefined
        return held(tx.merklePath, txid)
      } catch (e) {
        failed = `beef: ${(e as Error).message}`
      }
    } else {
      failed = `beef: ${res.status}`
    }
    const tsc = await this.get(`/tx/${txid}/proof/tsc`)
    if (tsc.status !== 200) throw new Error(`WhatsOnChain proof of ${txid}: ${failed}; tsc: ${tsc.status}`)
    const answer = (await tsc.json()) as TscProof[] | TscProof | null
    if (answer === null) return undefined
    const p = (Array.isArray(answer) ? answer : [answer]).find((x) => typeof x?.txOrId === 'string' && x.txOrId.toLowerCase() === txid.toLowerCase())
    if (p === undefined || !isTxid(p.target)) throw new Error(`WhatsOnChain's tsc answer holds no proof of ${txid}`)
    const hdr = await this.get(`/block/${p.target}/header`)
    if (hdr.status !== 200) throw new Error(`block ${p.target}: ${hdr.status}`)
    const h = (await hdr.json()) as { hash?: string; height?: number }
    if (h.hash?.toLowerCase() !== p.target.toLowerCase() || !Number.isSafeInteger(h.height)) throw new Error(`block ${p.target}: the answer is another block`)
    return held(tscMerklePath(p, txid, h.height as number), txid)
  }
}

/** A path that names txid at its leaf level, or a throw. */
export function held(mp: MerklePath, txid: string): MerklePath {
  if (!(mp.path[0] ?? []).some((l) => l.hash === txid.toLowerCase())) throw new Error('the proof does not contain the txid')
  return mp
}
