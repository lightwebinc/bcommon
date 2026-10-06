/**
 * The BRC-104 sessions a host's own listener keeps, bounded. Several
 * applications' hosts carried this as identical copies.
 */
import type { PeerSession } from '@bsv/sdk'

/** The default bound on BRC-104 sessions the terms route keeps, and their idle life. */
export const DefaultMaxSessions = 10_000
export const DefaultSessionTTL = 600

/**
 * The BRC-104 sessions the terms route keeps, bounded. The SDK's own store
 * keeps every session for the life of the process, and a handshake is
 * unauthenticated, so anyone could grow it without end. This one holds at
 * most `max` sessions and forgets one idle for `ttlMs` (idle: not added,
 * updated or looked up); past the cap it forgets the least recently used.
 * A client whose session was forgotten is refused (401) and shakes hands
 * again. It is the SessionManager contract the SDK's Peer uses.
 */
export class BoundedSessions {
  /** By session nonce, least recently used first. */
  private readonly byNonce = new Map<string, { s: PeerSession; at: number }>()
  private readonly byKey = new Map<string, Set<string>>()

  constructor(
    readonly max: number,
    readonly ttlMs: number,
    private readonly now: () => number = Date.now,
    private readonly evicted: (why: 'cap' | 'idle') => void = () => {},
  ) {
    if (!Number.isSafeInteger(max) || max < 1) throw new Error('BoundedSessions: max must be at least 1')
    if (!(ttlMs > 0)) throw new Error('BoundedSessions: ttlMs must be positive')
  }

  get size(): number {
    return this.byNonce.size
  }

  addSession(session: PeerSession): void {
    const nonce = session.sessionNonce
    if (typeof nonce !== 'string') throw new TypeError('Invalid session: sessionNonce is required to add a session.')
    this.forget(nonce)
    this.byNonce.set(nonce, { s: session, at: this.now() })
    if (typeof session.peerIdentityKey === 'string') {
      let set = this.byKey.get(session.peerIdentityKey)
      if (set === undefined) this.byKey.set(session.peerIdentityKey, (set = new Set()))
      set.add(nonce)
    }
    this.prune()
  }

  updateSession(session: PeerSession): void {
    this.addSession(session)
  }

  getSession(identifier: string): PeerSession | undefined {
    this.expire()
    const direct = this.byNonce.get(identifier)
    if (direct !== undefined) return this.touch(identifier, direct.s)
    let best: PeerSession | undefined
    for (const nonce of this.byKey.get(identifier) ?? []) {
      const e = this.byNonce.get(nonce)
      if (e !== undefined && (best === undefined || (e.s.lastUpdate ?? 0) > (best.lastUpdate ?? 0))) best = e.s
    }
    return best === undefined ? undefined : this.touch(best.sessionNonce!, best)
  }

  removeSession(session: PeerSession): void {
    if (typeof session.sessionNonce === 'string') this.forget(session.sessionNonce)
  }

  hasSession(identifier: string): boolean {
    return this.getSession(identifier) !== undefined
  }

  private touch(nonce: string, s: PeerSession): PeerSession {
    this.byNonce.delete(nonce)
    this.byNonce.set(nonce, { s, at: this.now() })
    return s
  }

  private forget(nonce: string): void {
    const e = this.byNonce.get(nonce)
    if (e === undefined) return
    this.byNonce.delete(nonce)
    const key = e.s.peerIdentityKey
    if (typeof key === 'string') {
      const set = this.byKey.get(key)
      set?.delete(nonce)
      if (set?.size === 0) this.byKey.delete(key)
    }
  }

  /** Forgets every session idle for ttlMs: the oldest come first. */
  private expire(): void {
    const cutoff = this.now() - this.ttlMs
    for (const [nonce, e] of this.byNonce) {
      if (e.at > cutoff) break
      this.forget(nonce)
      this.evicted('idle')
    }
  }

  private prune(): void {
    this.expire()
    for (const nonce of this.byNonce.keys()) {
      if (this.byNonce.size <= this.max) break
      this.forget(nonce)
      this.evicted('cap')
    }
  }
}
