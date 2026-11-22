/**
 * The BRC-104 sessions a host's own listener keeps, bounded. Several
 * applications' hosts carried this as identical copies.
 */
import type { PeerSession } from '@bsv/sdk'

/** The default bound on BRC-104 sessions the terms route keeps, and their idle life. */
export const DefaultMaxSessions = 10_000
export const DefaultSessionTTL = 600
/** The SDK 4 Peer's own bounds on one-time nonces, kept as they are. */
const MaxMessageNoncesPerSession = 100_000
const MaxInitialRequestNonces = 100_000
const MaxInitialRequestNoncesPerIdentity = 256

/**
 * The BRC-104 sessions the terms route keeps, bounded. The SDK's own store
 * keeps every session for the life of the process, and a handshake is
 * unauthenticated, so anyone could grow it without end. This one holds at
 * most `max` sessions and forgets one idle for `ttlMs` (idle: not added,
 * updated or looked up); past the cap it forgets the least recently used.
 * A client whose session was forgotten is refused (401) and shakes hands
 * again. It is the SessionManager contract the SDK's Peer uses, with the
 * two one-time nonce claims SDK 4 requires of a custom store (BRC-103 replay
 * protection): a signed message's nonce is taken once per session, and an
 * unsigned initial request's nonce once per identity for the session idle
 * life. Both are bounded here as the SDK's own manager bounds them.
 */
export class BoundedSessions {
  /** By session nonce, least recently used first. */
  private readonly byNonce = new Map<string, { s: PeerSession; at: number }>()
  private readonly byKey = new Map<string, Set<string>>()
  /** One-time message nonces by session nonce; dropped with the session. */
  private readonly messageNonces = new Map<string, Set<string>>()
  /** Initial-request claims by identity, then nonce, with the time taken. */
  private readonly initialNonces = new Map<string, Map<string, number>>()
  private initialCount = 0

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
    this.forget(nonce, true)
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

  /** Takes a signed message's nonce once; false when it was taken before. */
  claimMessageNonce(sessionNonce: string, messageNonce: string): boolean {
    if (typeof sessionNonce !== 'string' || sessionNonce === '') throw new TypeError('sessionNonce must be a non-empty string')
    if (typeof messageNonce !== 'string' || messageNonce === '') throw new TypeError('messageNonce must be a non-empty string')
    const s = this.getSession(sessionNonce)
    if (s === undefined || s.sessionNonce !== sessionNonce) throw new Error(`Session not found for nonce: ${sessionNonce}`)
    let taken = this.messageNonces.get(sessionNonce)
    if (taken === undefined) this.messageNonces.set(sessionNonce, (taken = new Set()))
    if (taken.has(messageNonce)) return false
    if (taken.size >= MaxMessageNoncesPerSession) throw new Error('BRC-103 session message nonce capacity exhausted')
    taken.add(messageNonce)
    return true
  }

  /**
   * Takes an unsigned initial request's nonce once per identity; false when
   * it was taken within the idle life. Past a bound the oldest claim goes,
   * as an unsigned sender must not be able to refuse every handshake.
   */
  claimInitialRequestNonce(identityKey: string, initialNonce: string): boolean {
    if (typeof identityKey !== 'string' || identityKey === '') throw new TypeError('identityKey must be a non-empty string')
    if (typeof initialNonce !== 'string' || initialNonce === '') throw new TypeError('initialNonce must be a non-empty string')
    const now = this.now()
    let mine = this.initialNonces.get(identityKey)
    const at = mine?.get(initialNonce)
    if (at !== undefined) {
      if (at > now - this.ttlMs) return false
      this.dropInitial(identityKey, initialNonce)
      mine = this.initialNonces.get(identityKey)
    }
    if (mine !== undefined && mine.size >= MaxInitialRequestNoncesPerIdentity) {
      this.dropInitial(identityKey, mine.keys().next().value as string)
      mine = this.initialNonces.get(identityKey)
    }
    if (this.initialCount >= MaxInitialRequestNonces) {
      for (const [k, m] of this.initialNonces) {
        this.dropInitial(k, m.keys().next().value as string)
        break
      }
      mine = this.initialNonces.get(identityKey)
    }
    if (mine === undefined) this.initialNonces.set(identityKey, (mine = new Map()))
    mine.set(initialNonce, now)
    this.initialCount++
    return true
  }

  private dropInitial(identityKey: string, nonce: string): void {
    const m = this.initialNonces.get(identityKey)
    if (m === undefined || !m.delete(nonce)) return
    this.initialCount--
    if (m.size === 0) this.initialNonces.delete(identityKey)
  }

  private touch(nonce: string, s: PeerSession): PeerSession {
    this.byNonce.delete(nonce)
    this.byNonce.set(nonce, { s, at: this.now() })
    return s
  }

  private forget(nonce: string, keepNonces = false): void {
    const e = this.byNonce.get(nonce)
    if (e === undefined) return
    this.byNonce.delete(nonce)
    if (!keepNonces) this.messageNonces.delete(nonce)
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
