/**
 * The value discriminator of the Go package acceptance, for a host that
 * takes a payment in TypeScript: a payment at or below the threshold is
 * 'fast', one above it is 'hold', and what the fast path has taken and not
 * seen mined is bounded per payer and in total within a window. The
 * network evidence (the host's own broadcast, a broadcaster's status, the
 * node's spend view) is the host's to gather; arcadeVerdict reads a
 * status the way the Go Verifier reads it.
 *
 * Satoshis are numbers, held to Number.MAX_SAFE_INTEGER as a host's prices
 * are. The zero policy holds every payment, and a price that is unknown,
 * zero or stale makes the threshold zero: every payment is held.
 */

export type AcceptanceDecision = 'refuse' | 'fast' | 'hold'

/** The reasons, the same labels as the Go package's. */
export type AcceptanceReason =
  | 'at-or-below-threshold'
  | 'above-threshold'
  | 'price-unknown'
  | 'price-stale'
  | 'payer-limit'
  | 'total-limit'
  | 'payer-flagged'
  | 'payer-asked-hold'
  | 'no-exposure-ledger'
  | 'no-txid'
  | 'already-charged'
  | 'pays-nothing'

export interface AcceptanceVerdict {
  decision: AcceptanceDecision
  reason: AcceptanceReason
  sats: number
  threshold: number
}

/** A coin's price in US cents, and when it was read (undefined: undated, never stale). */
export interface CoinPrice {
  centsPerCoin: number
  at?: number
}

/** Where a cents threshold gets its price; a rejection is an unknown price. */
export type PriceSource = () => Promise<CoinPrice>

export interface AcceptancePolicy {
  thresholdSats: number
  thresholdCents: number
  price?: PriceSource
  /** Milliseconds; zero never ages a price out. */
  maxPriceAge: number
  /** Milliseconds a fast payment counts unless it mines first; zero until released. */
  window: number
  payerLimit: number
  totalLimit: number
}

export const DefaultThresholdSats = 25_000_000
export const SatsPerCoin = 100_000_000

/** The Go DefaultPolicy. */
export function defaultAcceptancePolicy(): AcceptancePolicy {
  return {
    thresholdSats: DefaultThresholdSats,
    thresholdCents: 0,
    maxPriceAge: 3_600_000,
    window: 3_600_000,
    payerLimit: DefaultThresholdSats,
    totalLimit: 10 * DefaultThresholdSats,
  }
}

/** The zero policy: every payment held. */
export function zeroAcceptancePolicy(): AcceptancePolicy {
  return { thresholdSats: 0, thresholdCents: 0, maxPriceAge: 0, window: 0, payerLimit: 0, totalLimit: 0 }
}

/** The threshold now, and the reason it is zero when a price was needed and not known. */
export async function paymentThreshold(p: AcceptancePolicy, now: number = Date.now()): Promise<{ sats: number; reason?: AcceptanceReason }> {
  if (p.thresholdCents <= 0) return { sats: p.thresholdSats }
  if (p.price === undefined) return { sats: 0, reason: 'price-unknown' }
  let pr: CoinPrice
  try {
    pr = await p.price()
  } catch {
    return { sats: 0, reason: 'price-unknown' }
  }
  if (!Number.isSafeInteger(pr.centsPerCoin) || pr.centsPerCoin <= 0) return { sats: 0, reason: 'price-unknown' }
  if (p.maxPriceAge > 0 && pr.at !== undefined && now - pr.at > p.maxPriceAge) return { sats: 0, reason: 'price-stale' }
  const sats = Number((BigInt(p.thresholdCents) * BigInt(SatsPerCoin)) / BigInt(pr.centsPerCoin))
  const conv = Number.isSafeInteger(sats) ? sats : Number.MAX_SAFE_INTEGER
  return { sats: p.thresholdSats > 0 && p.thresholdSats < conv ? p.thresholdSats : conv }
}

interface Charge {
  payer: string
  sats: number
  at: number
}

/** What the fast path has taken and not seen mined, and the flagged payers. */
export class PaymentExposure {
  private readonly charges = new Map<string, Charge>()
  private readonly flagged = new Map<string, string>()

  /** Charges r if the limits allow it, or names the one that does not. */
  reserve(p: AcceptancePolicy, payer: string, txid: string, sats: number, now: number = Date.now()): AcceptanceReason | undefined {
    if (this.flagged.has(payer)) return 'payer-flagged'
    if (this.charges.has(txid)) return 'already-charged'
    let mine = 0
    let total = 0
    for (const [id, c] of this.charges) {
      if (p.window > 0 && now - c.at > p.window) {
        this.charges.delete(id)
        continue
      }
      total += c.sats
      if (c.payer === payer) mine += c.sats
    }
    if (mine + sats > p.payerLimit) return 'payer-limit'
    if (total + sats > p.totalLimit) return 'total-limit'
    this.charges.set(txid, { payer, sats, at: now })
    return undefined
  }

  charge(payer: string, txid: string, sats: number, at: number): void {
    this.charges.set(txid, { payer, sats, at })
  }

  release(txid: string): void {
    this.charges.delete(txid)
  }

  flag(payer: string, why: string): void {
    this.flagged.set(payer, why)
  }

  unflag(payer: string): void {
    this.flagged.delete(payer)
  }

  isFlagged(payer: string): boolean {
    return this.flagged.has(payer)
  }

  unmined(payer: string, window = 0, now: number = Date.now()): { payer: number; total: number } {
    let mine = 0
    let total = 0
    for (const c of this.charges.values()) {
      if (window > 0 && now - c.at > window) continue
      total += c.sats
      if (c.payer === payer) mine += c.sats
    }
    return { payer: mine, total }
  }
}

export interface PaymentRequest {
  payer: string
  txid: string
  sats: number
  ask?: 'fast' | 'hold'
}

/**
 * The Go Policy.Decide: on 'fast' the payment is charged to x at once, and
 * a host that then does not act on it calls x.release.
 */
export async function decidePayment(p: AcceptancePolicy, x: PaymentExposure | undefined, r: PaymentRequest, now: number = Date.now()): Promise<AcceptanceVerdict> {
  if (!Number.isSafeInteger(r.sats) || r.sats <= 0) return { decision: 'refuse', reason: 'pays-nothing', sats: 0, threshold: 0 }
  const th = await paymentThreshold(p, now)
  const hold = (reason: AcceptanceReason): AcceptanceVerdict => ({ decision: 'hold', reason, sats: r.sats, threshold: th.sats })
  if (th.reason !== undefined) return hold(th.reason)
  if (r.ask === 'hold') return hold('payer-asked-hold')
  if (r.sats > th.sats) return hold('above-threshold')
  if (x === undefined) return hold('no-exposure-ledger')
  if (r.txid === '') return hold('no-txid')
  const why = x.reserve(p, r.payer, r.txid, r.sats, now)
  if (why !== undefined) return hold(why)
  return { decision: 'fast', reason: 'at-or-below-threshold', sats: r.sats, threshold: th.sats }
}

/** A broadcaster's status as the fast path reads it. */
export type ArcadeVerdict = 'accepted' | 'conflict' | 'refused' | 'pending'

/**
 * An ARC status (txStatus, competingTxs) as the Go Verifier reads it:
 * REJECTED is refused; DOUBLE_SPEND_ATTEMPTED or any competing transaction
 * is a conflict, held for its block; a status the network has taken is
 * accepted; anything else is pending. Accepted is never enough alone: an
 * input the node shows spent by another transaction refuses the payment
 * whatever the broadcaster says.
 */
export function arcadeVerdict(st: { txStatus?: string; competingTxs?: readonly string[] | null }): ArcadeVerdict {
  const s = st.txStatus ?? ''
  if (s === 'REJECTED') return 'refused'
  if (s === 'DOUBLE_SPEND_ATTEMPTED' || (st.competingTxs?.length ?? 0) > 0) return 'conflict'
  switch (s) {
    case 'ACCEPTED_BY_NETWORK':
    case 'SEEN_ON_NETWORK':
    case 'SEEN_MULTIPLE_NODES':
    case 'STUMP_PROCESSING':
    case 'MINED':
    case 'IMMUTABLE':
      return 'accepted'
  }
  return 'pending'
}
