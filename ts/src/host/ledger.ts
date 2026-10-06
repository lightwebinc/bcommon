/**
 * The payee ledger as a host writes and reads it: one line a payment the
 * host accepted, in `payments.jsonl` in its state directory, flushed to disk
 * before the question it paid for is answered. The lines are the Go `payee`
 * package's, version 2 (`payee.LedgerV2`): txid, beef (Atomic BEEF, standard
 * base64), outputIndex, satoshis, derivationPrefix, derivationSuffix,
 * senderIdentityKey, class, inputs and at (Unix seconds), in that order, so
 * the payee's settle tooling reads, checks, broadcasts and takes them as it
 * takes any host's. A line the host wrote with its decision also carries
 * `decision` and `reason`, which the Go reader passes over.
 *
 * Two layouts exist, because applications wrote the decision in two places
 * and a ledger's bytes are kept as written: `payee` puts it after `at`,
 * `spread` before it. A line with no decision is the same in both, and is
 * byte for byte what Go's `payee.Payment.Line` writes for the same payment.
 */
import { closeSync, fdatasyncSync, mkdirSync, openSync, readFileSync, writeSync } from 'node:fs'
import { join } from 'node:path'
import { Transaction, Utils } from '@bsv/sdk'

/** The ledger's name in a host's state directory (Go `payee.LedgerFile`). */
export const LedgerFile = 'payments.jsonl'

/** A payment the route accepted: what a wallet's internalizeAction needs to take it. */
export interface ReceivedPayment {
  txid: string
  /** The payment as Atomic BEEF. */
  beef: number[]
  outputIndex: 0
  satoshis: number
  derivationPrefix: string
  derivationSuffix: string
  senderIdentityKey: string
  class: string
  /** Every outpoint the payment spends, `<txid>.<index>`, txid in display order. */
  inputs: string[]
  /** What the host did: fast and mined are answered, hold is not. */
  decision?: 'fast' | 'hold' | 'mined'
  reason?: string
}

/** Every outpoint a transaction spends, `<txid>.<index>`. */
export function spentOutpoints(tx: Transaction): string[] {
  return tx.inputs.map((i) => `${i.sourceTXID ?? i.sourceTransaction?.id('hex') ?? ''}.${i.sourceOutputIndex}`)
}

/**
 * Where a line puts the host's decision: `payee` after `at`, `spread`
 * before it. `spread` is `{ ...payment, beef, at }`, so it writes the
 * payment's own key order: the route builds a payment in the order of
 * ReceivedPayment, and then the two are the same without a decision.
 */
export type LedgerLayout = 'payee' | 'spread'

/** A payment as a ledger line, newline included. at is Unix seconds. */
export function ledgerLine(p: ReceivedPayment, at: number, layout: LedgerLayout = 'payee'): string {
  if (layout === 'spread') return JSON.stringify({ ...p, beef: Utils.toBase64(Array.from(Uint8Array.from(p.beef))), at }) + '\n'
  const line = {
    txid: p.txid,
    beef: Utils.toBase64(Array.from(Uint8Array.from(p.beef))),
    outputIndex: p.outputIndex,
    satoshis: p.satoshis,
    derivationPrefix: p.derivationPrefix,
    derivationSuffix: p.derivationSuffix,
    senderIdentityKey: p.senderIdentityKey,
    class: p.class,
    inputs: p.inputs,
    at,
    // What the host decided; a line without it is the payee's own line, byte for byte.
    ...(p.decision === undefined ? {} : { decision: p.decision, reason: p.reason }),
  }
  return JSON.stringify(line) + '\n'
}

/** A ledger line as read: the keys the host itself reads, unchecked beyond their types. */
export interface LedgerLine {
  txid: string
  /** The line's own inputs, or else those read from its transaction; [] when neither can be read. */
  inputs: string[]
  decision?: 'fast' | 'hold' | 'mined'
  beef?: string
  senderIdentityKey?: string
  satoshis?: number
  /** Unix seconds. */
  at?: number
}

/**
 * Reads one ledger line as a host reads it at start: JSON with a string
 * txid, or undefined (a line cut short by a crash, whose payment was never
 * answered). A line written before lines carried their inputs has them read
 * from its transaction. Go's `payee.ParseLine` also refuses an empty txid;
 * a host never writes one.
 */
export function readLedgerLine(l: string): LedgerLine | undefined {
  let v: { txid?: unknown; inputs?: unknown; beef?: unknown; decision?: unknown; senderIdentityKey?: unknown; satoshis?: unknown; at?: unknown }
  try {
    v = JSON.parse(l) as typeof v
  } catch {
    return undefined
  }
  if (typeof v?.txid !== 'string') return undefined
  let inputs: string[] = []
  if (Array.isArray(v.inputs) && v.inputs.every((i) => typeof i === 'string')) {
    inputs = v.inputs as string[]
  } else if (typeof v.beef === 'string') {
    try {
      inputs = spentOutpoints(Transaction.fromAtomicBEEF(Utils.toArray(v.beef, 'base64')))
    } catch {
      // the txid alone is claimed
    }
  }
  const out: LedgerLine = { txid: v.txid, inputs }
  if (v.decision === 'fast' || v.decision === 'hold' || v.decision === 'mined') out.decision = v.decision
  if (typeof v.beef === 'string') out.beef = v.beef
  if (typeof v.senderIdentityKey === 'string') out.senderIdentityKey = v.senderIdentityKey
  if (typeof v.satoshis === 'number') out.satoshis = v.satoshis
  if (typeof v.at === 'number') out.at = v.at
  return out
}

/** What a receiver says of a payment offered to it. */
export type Claim = 'accepted' | 'replayed' | 'conflict'

/**
 * The payments a receiver took, by txid and by the coins they spend. Two
 * transactions spending one coin can both verify, and at most one can ever
 * be settled: the second is a conflict, whatever its txid. A payment held
 * for confirmation keeps its coins, and is answered once, when it mines.
 */
export class TakenPayments {
  private readonly answered = new Set<string>()
  private readonly held = new Set<string>()
  private readonly coins = new Map<string, string>()

  /** Why p cannot be taken, or undefined. */
  refusal(p: Pick<ReceivedPayment, 'txid' | 'inputs'>): Exclude<Claim, 'accepted'> | undefined {
    if (this.answered.has(p.txid)) return 'replayed'
    if (p.inputs.length === 0 || p.inputs.some((i) => (this.coins.get(i) ?? p.txid) !== p.txid)) return 'conflict'
    return undefined
  }

  take(p: Pick<ReceivedPayment, 'txid' | 'inputs' | 'decision'>): void {
    if (p.decision === 'hold') {
      this.held.add(p.txid)
    } else {
      this.held.delete(p.txid)
      this.answered.add(p.txid)
    }
    for (const i of p.inputs) this.coins.set(i, p.txid)
  }

  isHeld(txid: string): boolean {
    return this.held.has(txid)
  }
}

/**
 * Where accepted payments go. claim records p and answers `accepted`,
 * atomically, unless its txid was claimed before (`replayed`) or it spends
 * a coin an accepted payment spent (`conflict`): one payment buys one
 * question, and one coin pays once.
 */
export interface PaymentReceiver {
  /** Why p cannot be taken, before it is broadcast; undefined when it can. */
  refusal(p: Pick<ReceivedPayment, 'txid' | 'inputs'>): Exclude<Claim, 'accepted'> | undefined
  claim(p: ReceivedPayment): Claim
}

/** Payments in memory, for tests. */
export class MemoryReceiver implements PaymentReceiver {
  readonly payments: ReceivedPayment[] = []
  private readonly taken = new TakenPayments()
  refusal(p: Pick<ReceivedPayment, 'txid' | 'inputs'>): Exclude<Claim, 'accepted'> | undefined {
    return this.taken.refusal(p)
  }
  claim(p: ReceivedPayment): Claim {
    const no = this.taken.refusal(p)
    if (no !== undefined) return no
    this.taken.take(p)
    this.payments.push(p)
    return 'accepted'
  }
}

export interface LedgerOptions {
  /** The host's clock, Unix milliseconds; Date.now when unset. */
  now?: () => number
  /** Where a line puts the decision; `payee` when unset. */
  layout?: LedgerLayout
}

/**
 * Payments as a ledger file, `payments.jsonl` in dir, one line each, flushed
 * to disk before the question is answered: what the operator's wallet
 * internalizes (BRC-100 internalizeAction, protocol "wallet payment", output
 * 0, with the prefix, the suffix and the sender's identity key). The txids
 * already in the file are claimed, and so are the coins they spend: each
 * line carries its payment's inputs, and a line written before it did is
 * read from its transaction.
 */
export class LedgerReceiver implements PaymentReceiver {
  readonly path: string
  private readonly taken = new TakenPayments()
  private fd: number | undefined
  private readonly now: () => number
  private readonly layout: LedgerLayout
  /** The fast lines read at start, newest last: watched again until they mine. */
  readonly fast: Array<{ beef: string; senderIdentityKey: string; satoshis: number; at: number }> = []

  constructor(dir: string, o: LedgerOptions = {}) {
    this.now = o.now ?? Date.now
    this.layout = o.layout ?? 'payee'
    mkdirSync(dir, { recursive: true })
    this.path = join(dir, LedgerFile)
    let text = ''
    try {
      text = readFileSync(this.path, 'utf8')
    } catch (e) {
      if ((e as NodeJS.ErrnoException).code !== 'ENOENT') throw e
    }
    for (const l of text.split('\n')) {
      const v = readLedgerLine(l)
      if (v === undefined) continue
      this.taken.take({ txid: v.txid, inputs: v.inputs, decision: v.decision === 'hold' ? 'hold' : undefined })
      if (v.decision === 'fast' && v.beef !== undefined && v.senderIdentityKey !== undefined && v.satoshis !== undefined && v.at !== undefined) {
        this.fast.push({ beef: v.beef, senderIdentityKey: v.senderIdentityKey, satoshis: v.satoshis, at: v.at * 1000 })
      }
    }
    if (text.length > 0 && !text.endsWith('\n')) this.write('\n')
  }

  private write(s: string): void {
    this.fd ??= openSync(this.path, 'a')
    writeSync(this.fd, s)
    fdatasyncSync(this.fd)
  }

  refusal(p: Pick<ReceivedPayment, 'txid' | 'inputs'>): Exclude<Claim, 'accepted'> | undefined {
    return this.taken.refusal(p)
  }

  claim(p: ReceivedPayment): Claim {
    const no = this.taken.refusal(p)
    if (no !== undefined) return no
    const again = this.taken.isHeld(p.txid) && p.decision === 'hold'
    this.taken.take(p)
    if (again) return 'accepted'
    this.write(ledgerLine(p, Math.floor(this.now() / 1000), this.layout))
    return 'accepted'
  }

  close(): void {
    if (this.fd !== undefined) closeSync(this.fd)
    this.fd = undefined
  }
}
