/**
 * Test helpers for an application's overlay modules: a host whose metrics and
 * log can be read back, the rows and storage a restore reads, BEEF built the
 * way the engine builds it, a minter over a fixed test key, and a simulator
 * that calls a lookup service's callbacks in the order the engine makes them;
 * a payment network (arcade and the node) in memory and over HTTP
 * (network.ts); and the checks a host module's bundle step applies to
 * esbuild's metafile (bundle.ts).
 *
 * This is the package's `./testing` entry point, for tests running on Node.
 * The runtime entry point never imports it.
 */
import {
  Hash,
  PrivateKey,
  ProtoWallet,
  Transaction,
  Utils,
  type LockingScript,
  type WalletInterface,
} from '@bsv/sdk'
import type { LookupService, ModuleHost, Output, RestoreStorage } from '../engine-types.js'

export function fromHex(hex: string): Uint8Array {
  return Uint8Array.from(Utils.toArray(hex, 'hex') as number[])
}

export function toHex(b: Uint8Array | number[]): string {
  return Utils.toHex(b instanceof Uint8Array ? Array.from(b) : b)
}

/** n bytes of b. */
export function fill(b: number, n = 32): Uint8Array {
  return new Uint8Array(n).fill(b)
}

export function sha256(b: Uint8Array): Uint8Array {
  return Uint8Array.from(Hash.sha256(Array.from(b)))
}

/** A host whose metrics can be read back, and whose log is kept. */
export interface CountingHost extends ModuleHost {
  readonly counters: Map<string, number>
  readonly gauges: Map<string, () => number>
  readonly logged: Array<{ msg: string; extra?: Record<string, unknown> }>
  /** A counter's value, or -1 when it was never incremented or preset. */
  count: (name: string, labels?: Record<string, string>) => number
}

export function countingHost(): CountingHost {
  const counters = new Map<string, number>()
  const gauges = new Map<string, () => number>()
  const logged: CountingHost['logged'] = []
  const key = (name: string, labels: Record<string, string> = {}): string =>
    `${name}{${Object.entries(labels)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, v]) => `${k}="${v}"`)
      .join(',')}}`
  return {
    counters,
    gauges,
    logged,
    log: (msg, extra) => {
      logged.push({ msg, extra })
    },
    metrics: {
      inc: (name, labels, by = 1) => {
        counters.set(key(name, labels), (counters.get(key(name, labels)) ?? 0) + by)
      },
      preset: (name, labels) => {
        if (!counters.has(key(name, labels))) counters.set(key(name, labels), 0)
      },
      gauge: (name, fn) => {
        gauges.set(name, fn)
      },
    },
    count: (name, labels) => counters.get(key(name, labels)) ?? -1,
  }
}

/** Attach parent as the source transaction of every input that spends it. */
export function wireParent(tx: Transaction, parent: Transaction): void {
  const id = parent.id('hex')
  for (const input of tx.inputs) {
    if (input.sourceTXID === id) input.sourceTransaction = parent
  }
}

/**
 * BEEF for a topic manager. allowPartial, because a test transaction's input
 * may have no source transaction here; the engine's SPV step is not what a
 * module's unit tests exercise.
 */
export function beefOf(tx: Transaction): number[] {
  return tx.toBEEF(true)
}

/**
 * Atomic BEEF for a lookup service's whole-tx admission payload, built the
 * way the engine builds it (tx.toAtomicBEEF()), partial allowed for the same
 * reason as beefOf.
 */
export function atomicOf(tx: Transaction): number[] {
  return tx.toAtomicBEEF(true)
}

export interface Minter {
  priv: PrivateKey
  identityKeyHex: string
  wallet: WalletInterface
}

/**
 * A wallet over a fixed test key. ProtoWallet implements the key operations
 * and not the action surface, so it is not a WalletInterface to the type
 * checker; PushDrop.lock calls only getPublicKey and createSignature, which
 * it has.
 */
export function minter(privHex: string): Minter {
  const priv = PrivateKey.fromHex(privHex)
  return { priv, identityKeyHex: priv.toPublicKey().toString(), wallet: new ProtoWallet(priv) as unknown as WalletInterface }
}

/**
 * An outputs-table row for restore, as findUTXOsForTopic returns it.
 * `outputsConsumed` is what the engine fills from the previous coins the
 * topic manager retained; `consumedBy` is who spent this output, as a fake
 * storage's findOutput would answer.
 */
export function row(
  tx: Transaction,
  outputIndex: number,
  topic: string,
  spent = false,
  links: Partial<Pick<Output, 'outputsConsumed' | 'consumedBy'>> = {},
): Output {
  const out = tx.outputs[outputIndex]
  if (out === undefined) throw new Error(`no output ${outputIndex}`)
  return {
    txid: tx.id('hex'),
    outputIndex,
    outputScript: (out.lockingScript as LockingScript).toBinary(),
    satoshis: out.satoshis ?? 1,
    topic,
    spent,
    outputsConsumed: links.outputsConsumed ?? [],
    consumedBy: links.consumedBy ?? [],
  }
}

/** The outpoints a transaction spends, as a row's outputsConsumed lists them. */
export function spends(tx: Transaction): Array<{ txid: string; outputIndex: number }> {
  return tx.inputs.map((i) => ({ txid: i.sourceTXID ?? i.sourceTransaction?.id('hex') ?? '', outputIndex: i.sourceOutputIndex }))
}

/**
 * A RestoreStorage over a fixed set of rows keyed by outpoint. Answers null
 * for anything else, as the engine's storage does for an output it never
 * held.
 */
export function fakeStorage(rows: Output[]): RestoreStorage {
  const byKey = new Map(rows.map((r) => [`${r.txid}.${r.outputIndex}`, r]))
  return { findOutput: async (txid, outputIndex) => byKey.get(`${txid}.${outputIndex}`) ?? null }
}

/**
 * What a simulated call returns: nothing when every callback it made
 * returned synchronously, so a synchronous service's tests need no await,
 * and otherwise a promise that settles once the last of them has.
 */
export type Step = Promise<void> | void

/**
 * A lookup service's callbacks driven in the order the engine makes them
 * when it admits a transaction: a previous output is reported spent BEFORE
 * the new outputs are admitted, which is the caller's to follow, and each
 * call carries the payload the service's declared admissionMode and
 * spendNotificationMode ask for. A callback's error propagates, where the
 * engine would log it and carry on, so that a test sees it.
 */
export interface EngineOrder {
  /** One output admitted. */
  admit: (tx: Transaction, outputIndex?: number) => Step
  /** Every output of the transaction admitted, in output order. */
  admitAll: (tx: Transaction) => Step
  /** Output outputIndex of spent reported spent by the transaction by. */
  spend: (spent: Transaction, by: Transaction, outputIndex?: number) => Step
  /**
   * The outpoint that input inputIndex of tx spends reported spent by the
   * transaction by: a spend of an output that was never admitted here.
   */
  spendInput: (tx: Transaction, by: Transaction, inputIndex?: number) => Step
}

/** Run the steps in order, synchronously for as long as each one is. */
function inOrder(steps: Array<() => Step>): Step {
  for (let i = 0; i < steps.length; i++) {
    const r = steps[i]!()
    if (r instanceof Promise) return r.then(() => inOrder(steps.slice(i + 1)))
  }
  return undefined
}

export function engineOrder(ls: LookupService, topic: string): EngineOrder {
  const admit = (tx: Transaction, outputIndex = 0): Step => {
    const out = tx.outputs[outputIndex]
    if (out === undefined) throw new Error(`no output ${outputIndex}`)
    // The engine stores and reports only an output with a value, so a test
    // admitting one without is simulating something that cannot happen.
    if (out.satoshis === undefined) throw new Error(`output ${outputIndex} has no satoshis; the engine would not admit it`)
    if (ls.admissionMode === 'locking-script') {
      return ls.outputAdmittedByTopic({
        mode: 'locking-script',
        txid: tx.id('hex'),
        outputIndex,
        topic,
        satoshis: out.satoshis,
        lockingScript: out.lockingScript,
      })
    }
    return ls.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: atomicOf(tx), outputIndex, topic })
  }

  const spendOutpoint = (txid: string, outputIndex: number, by: Transaction): Step => {
    if (ls.outputSpent === undefined) return undefined
    const spendingTxid = by.id('hex')
    switch (ls.spendNotificationMode) {
      case 'txid':
        return ls.outputSpent({ mode: 'txid', txid, outputIndex, topic, spendingTxid })
      case 'script': {
        const inputIndex = by.inputs.findIndex(
          (i) => (i.sourceTXID ?? i.sourceTransaction?.id('hex')) === txid && i.sourceOutputIndex === outputIndex,
        )
        const input = by.inputs[inputIndex]
        if (input?.unlockingScript === undefined) throw new Error(`${spendingTxid} has no signed input spending ${txid}.${outputIndex}`)
        return ls.outputSpent({
          mode: 'script',
          txid,
          outputIndex,
          topic,
          spendingTxid,
          inputIndex,
          unlockingScript: input.unlockingScript,
          sequenceNumber: input.sequence ?? 0xffffffff,
        })
      }
      case 'whole-tx':
        return ls.outputSpent({ mode: 'whole-tx', txid, outputIndex, topic, spendingAtomicBEEF: atomicOf(by) })
      default:
        return ls.outputSpent({ mode: 'none', txid, outputIndex, topic })
    }
  }

  return {
    admit,
    admitAll: (tx) => inOrder(tx.outputs.map((_, i) => () => admit(tx, i))),
    spend: (spent, by, outputIndex = 0) => spendOutpoint(spent.id('hex'), outputIndex, by),
    spendInput: (tx, by, inputIndex = 0) => {
      const input = tx.inputs[inputIndex]
      const txid = input?.sourceTXID ?? input?.sourceTransaction?.id('hex')
      if (input === undefined || txid === undefined) throw new Error(`no input ${inputIndex} with a source txid`)
      return spendOutpoint(txid, input.sourceOutputIndex, by)
    },
  }
}

export { type ArcadeMode, TestNetwork, serveNetwork } from './network.js'
export { type BundleRules, type CheckedMetafile, bundleRefusals } from './bundle.js'
