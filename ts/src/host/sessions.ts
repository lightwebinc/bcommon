/**
 * The BRC-104 sessions a host's own listener keeps, bounded. Several
 * applications' hosts carried this as identical copies.
 */
import type { PeerSession } from '@bsv/sdk'

/** The default bound on BRC-104 sessions the terms route keeps, and their idle life. */
export const DefaultMaxSessions = 10_000
export const DefaultSessionTTL = 600
/**
 * The bounds on one-time nonces. The first two and the last two are the SDK's
 * own (SessionManager, 4.1.2); the SDK's claim life is its session idle life,
 * 30 minutes. The total over all sessions is this module's: the SDK bounds a
 * session's nonces but not their sum.
 */
export const DefaultNonceLimits = {
  messageNoncesPerSession: 100_000,
  messageNoncesTotal: 250_000,
  initialNonces: 100_000,
  initialNoncesPerIdentity: 256,
  initialNonceLifeMs: 30 * 60 * 1000,
}
export type NonceLimits = typeof DefaultNonceLimits

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
 * unsigned initial request's nonce once per identity for the SDK's claim
 * life. Both are bounded. A message nonce is never dropped on its own, since
 * that would let a signed message be replayed in its session: past the total
 * the least recently used session goes whole, and its client shakes hands
 * again. A claim on a session this store no longer holds is refused, so the
 * Peer answers as it does for any replay.
 */
export class BoundedSessions {
  /** By session nonce, least recently used first. */
  private readonly byNonce = new Map<string, { s: PeerSession; at: number }>()
  private readonly byKey = new Map<string, Set<string>>()
  /** One-time message nonces by session nonce; dropped with the session. */
  private readonly messageNonces = new Map<string, Set<string>>()
  private messageCount = 0
  /** Initial-request claims, key `identity NUL nonce`, to the time taken; oldest first. */
  private readonly initialClaims = new Map<string, number>()
  /** The same keys by identity, oldest first. */
  private readonly initialByIdentity = new Map<string, Set<string>>()
  private readonly limits: NonceLimits

  constructor(
    readonly max: number,
    readonly ttlMs: number,
    private readonly now: () => number = Date.now,
    private readonly evicted: (why: 'cap' | 'idle') => void = () => {},
    limits: Partial<NonceLimits> = {},
  ) {
    this.limits = { ...DefaultNonceLimits, ...limits }
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

  /**
   * Takes a signed message's nonce once. False when it was taken before, and
   * when this store holds no such session (never added, idle, or evicted):
   * the claim does not refresh the session's idle life.
   */
  claimMessageNonce(sessionNonce: string, messageNonce: string): boolean {
    if (typeof sessionNonce !== 'string' || sessionNonce === '') throw new TypeError('sessionNonce must be a non-empty string')
    if (typeof messageNonce !== 'string' || messageNonce === '') throw new TypeError('messageNonce must be a non-empty string')
    this.expire()
    if (!this.byNonce.has(sessionNonce)) return false
    let taken = this.messageNonces.get(sessionNonce)
    if (taken?.has(messageNonce) === true) return false
    if ((taken?.size ?? 0) >= this.limits.messageNoncesPerSession) throw new Error('BRC-103 session message nonce capacity exhausted')
    while (this.messageCount >= this.limits.messageNoncesTotal) {
      const oldest = [...this.byNonce.keys()].find((n) => n !== sessionNonce)
      // The claiming session is the only one left: its own nonces fill the total.
      if (oldest === undefined) throw new Error('BRC-103 message nonce capacity exhausted')
      this.forget(oldest)
      this.evicted('cap')
    }
    if (taken === undefined) this.messageNonces.set(sessionNonce, (taken = new Set()))
    taken.add(messageNonce)
    this.messageCount++
    return true
  }

  /**
   * Takes an unsigned initial request's nonce once per identity. False when
   * it was taken within the claim life. Claims older than that go first; past
   * a bound the oldest claim goes, as an unsigned sender must not be able to
   * refuse every handshake.
   */
  claimInitialRequestNonce(identityKey: string, initialNonce: string): boolean {
    if (typeof identityKey !== 'string' || identityKey === '') throw new TypeError('identityKey must be a non-empty string')
    if (typeof initialNonce !== 'string' || initialNonce === '') throw new TypeError('initialNonce must be a non-empty string')
    const now = this.now()
    const cutoff = now - this.limits.initialNonceLifeMs
    for (const [k, at] of this.initialClaims) {
      if (at > cutoff) break
      this.dropInitial(k)
    }
    const key = `${identityKey}\u0000${initialNonce}`
    if (this.initialClaims.has(key)) return false
    const mine = this.initialByIdentity.get(identityKey)
    if (mine !== undefined && mine.size >= this.limits.initialNoncesPerIdentity) this.dropInitial(mine.values().next().value as string)
    if (this.initialClaims.size >= this.limits.initialNonces) this.dropInitial(this.initialClaims.keys().next().value as string)
    this.initialClaims.set(key, now)
    let set = this.initialByIdentity.get(identityKey)
    if (set === undefined) this.initialByIdentity.set(identityKey, (set = new Set()))
    set.add(key)
    return true
  }

  /** Claims held, for tests and gauges. */
  get claims(): { messages: number; initial: number } {
    return { messages: this.messageCount, initial: this.initialClaims.size }
  }

  private dropInitial(key: string): void {
    if (!this.initialClaims.delete(key)) return
    const id = key.slice(0, key.indexOf('\u0000'))
    const set = this.initialByIdentity.get(id)
    set?.delete(key)
    if (set?.size === 0) this.initialByIdentity.delete(id)
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
    if (!keepNonces) {
      this.messageCount -= this.messageNonces.get(nonce)?.size ?? 0
      this.messageNonces.delete(nonce)
    }
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
