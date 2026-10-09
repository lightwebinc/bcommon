/**
 * A host module's block headers: a chain tracker over a header source's
 * native `/v1` routes (`/v1/root/<height>`, `/v1/tip`), the shape the
 * reference overlay host's own tracker reads. A module needs its own because
 * the reference host hands a module no tracker, and admission checks a
 * token's parent's proof, which the engine's SPV of the token does not reach.
 *
 * A height the source does not hold is not valid; any other failure is an
 * error, which a topic manager turns into a refusal the engine records
 * nothing for. A root the source confirmed is reused for ten minutes, as the
 * reference host's own source reuses proven roots. Every answer is read to
 * at most MaxHeaderAnswer bytes, and a redirect is an error.
 */
import type { ChainTracker } from '@bsv/sdk'

const RootTTL = 10 * 60_000
const RootCacheMax = 10_000
/** The most bytes a header source's answer may hold: a root or a tip is a few hundred. */
export const MaxHeaderAnswer = 64 << 10

/** Reads a response body to at most max bytes, or throws: the bound an answer is held to. */
export async function readCapped(res: Response, max: number): Promise<string> {
  if (res.body === null) return ''
  const reader = res.body.getReader()
  const parts: Uint8Array[] = []
  let n = 0
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    n += value.length
    if (n > max) {
      await reader.cancel()
      throw new Error(`the answer is over ${max} bytes`)
    }
    parts.push(value)
  }
  const out = new Uint8Array(n)
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.length
  }
  return new TextDecoder().decode(out)
}

export class HeaderTracker implements ChainTracker {
  private readonly proven = new Map<string, number>()

  constructor(
    readonly base: string,
    private readonly timeoutMs = 10_000,
    private readonly now: () => number = Date.now,
  ) {
    let u: URL
    try {
      u = new URL(base)
    } catch {
      throw new Error(`headers: ${base} is not a URL`)
    }
    if (u.protocol !== 'http:' && u.protocol !== 'https:') throw new Error(`headers: ${base} is not an http or https URL`)
  }

  private async get(path: string): Promise<{ status: number; body: unknown }> {
    const res = await fetch(`${this.base.replace(/\/+$/, '')}${path}`, { headers: { accept: 'application/json' }, redirect: 'error', signal: AbortSignal.timeout(this.timeoutMs) })
    const text = await readCapped(res, MaxHeaderAnswer)
    if (res.status !== 200) return { status: res.status, body: undefined }
    try {
      return { status: res.status, body: JSON.parse(text) as unknown }
    } catch {
      throw new Error(`headers: ${path}: the answer is not JSON`)
    }
  }

  async isValidRootForHeight(root: string, height: number): Promise<boolean> {
    const key = `${height}:${root}`
    const at = this.proven.get(key)
    if (at !== undefined && this.now() - at < RootTTL) return true
    const { status, body } = await this.get(`/v1/root/${height}`)
    if (status === 404) return false
    if (status !== 200) throw new Error(`headers: root for height ${height}: status ${status}`)
    const ok = (body as { merkleRoot?: unknown }).merkleRoot === root
    if (ok) {
      this.proven.delete(key)
      this.proven.set(key, this.now())
      if (this.proven.size > RootCacheMax) this.proven.delete(this.proven.keys().next().value as string)
    }
    return ok
  }

  async currentHeight(): Promise<number> {
    const { status, body } = await this.get('/v1/tip')
    const height = (body as { height?: unknown } | undefined)?.height
    if (status !== 200 || typeof height !== 'number') throw new Error(`headers: tip: status ${status}`)
    return height
  }
}

/** Whether s names a native header source a module can read: an http or https URL. */
export function isNativeSource(s: string): boolean {
  try {
    const u = new URL(s)
    return (u.protocol === 'http:' || u.protocol === 'https:') && u.host !== ''
  } catch {
    return false
  }
}
