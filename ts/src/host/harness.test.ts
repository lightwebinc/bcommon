/**
 * Helpers for the host entry point's tests: a test chain whose blocks hold
 * one transaction each, a wallet that pays from coins mined on it, arcade and
 * the node in memory, and a terms route on a free port in front of a lookup
 * service that answers a fixed list. No tests of its own.
 */
import type { AddressInfo } from 'node:net'
import type { Server } from 'node:http'
import {
  LockingScript,
  MerklePath,
  P2PKH,
  PrivateKey,
  ProtoWallet,
  Transaction,
  UnlockingScript,
  type ChainTracker,
  type CreateActionArgs,
  type CreateActionResult,
} from '@bsv/sdk'
import { defaultAcceptancePolicy, type AcceptancePolicy } from '../acceptance.js'
import { countingHost, type CountingHost } from '../testing/index.js'
import { BroadcastRefused, PaymentGate, type ArcadeStatus, type Broadcaster, type NodeView } from './accept.js'
import type { BudgetConfig, ResponseBudgetConfig } from './budget.js'
import { LookupFront, parsePrices, type LookupBody, type PricedClass } from './front.js'
import { MemoryReceiver } from './ledger.js'

export const Classes: PricedClass[] = [
  { name: 'list', free: true },
  { name: 'history', free: false },
  { name: 'history-after', free: false },
]
export const Service = 'ls_example'
export const Protocol: [2, string] = [2, '3241645161d8']

export class Chain {
  readonly roots = new Map<number, string>()
  constructor(private height = 5000) {}

  /** Gives tx a proof in a new block and returns it. */
  mine(tx: Transaction): Transaction {
    const height = this.height++
    tx.merklePath = new MerklePath(height, [
      [
        { offset: 0, hash: '55'.repeat(32) },
        { offset: 1, hash: tx.id('hex'), txid: true },
      ],
    ])
    this.roots.set(height, tx.merklePath.computeRoot(tx.id('hex')))
    return tx
  }

  get tracker(): ChainTracker {
    return {
      isValidRootForHeight: async (root, height) => this.roots.get(height) === root,
      currentHeight: async () => Math.max(...this.roots.keys()),
    }
  }
}

let nowhere = 0
/** A transaction spending an output nobody holds. */
export function fromNowhere(outs: Array<{ satoshis: number; lockingScript: LockingScript }>): Transaction {
  const id = (++nowhere).toString(16).padStart(64, '0')
  return new Transaction(1, [{ sourceTXID: id, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff }], outs, 0)
}

export class PayingWallet extends ProtoWallet {
  constructor(
    readonly key: PrivateKey,
    private readonly chain: Chain,
  ) {
    super(key)
  }

  coin(): Transaction {
    return this.chain.mine(fromNowhere([{ satoshis: 100000, lockingScript: new P2PKH().lock(this.key.toAddress()) }]))
  }

  async pay(outputs: Array<{ satoshis: number; lockingScript: string }>, coin = this.coin()): Promise<Transaction> {
    const tx = new Transaction(
      1,
      [{ sourceTransaction: coin, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(this.key), sequence: 0xffffffff }],
      [
        ...outputs.map((o) => ({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.lockingScript) })),
        { satoshis: 100000 - outputs.reduce((n, o) => n + o.satoshis, 0) - 100, lockingScript: new P2PKH().lock(this.key.toAddress()) },
      ],
      0,
    )
    await tx.sign()
    return tx
  }

  async createAction(args: CreateActionArgs): Promise<CreateActionResult> {
    const tx = await this.pay((args.outputs ?? []).map((o) => ({ satoshis: o.satoshis, lockingScript: o.lockingScript })))
    return { tx: tx.toAtomicBEEF(), txid: tx.id('hex') }
  }
}

export type ArcadeMode = 'accept' | 'double-spend' | 'reject' | 'refuse-http' | 'silent' | 'down'

/** arcade and the node's asset service in memory, over a test chain. */
export class TestNetwork implements Broadcaster, NodeView {
  mode: ArcadeMode = 'accept'
  spendUnknown = false
  readonly sent: string[] = []
  private readonly txs = new Map<string, Transaction>()
  private readonly statuses = new Map<string, ArcadeStatus>()
  private readonly spent = new Map<string, string>()
  private readonly proofs = new Map<string, MerklePath>()

  constructor(private readonly chain: Chain) {}

  async submit(ef: Uint8Array): Promise<ArcadeStatus> {
    if (this.mode === 'down') throw new Error('arcade unreachable')
    const tx = Transaction.fromEF(Array.from(ef))
    const txid = tx.id('hex')
    this.sent.push(txid)
    if (this.mode === 'refuse-http') throw new BroadcastRefused('arcade answered 465: fee too low')
    if (this.statuses.has(txid)) return this.statuses.get(txid)!
    let st: ArcadeStatus
    const competing = tx.inputs.map((i) => this.spent.get(`${i.sourceTXID}.${i.sourceOutputIndex}`)).filter((s): s is string => s !== undefined && s !== txid)
    if (this.mode === 'reject') st = { txid, txStatus: 'REJECTED', extraInfo: 'missing inputs' }
    else if (this.mode === 'double-spend' || competing.length > 0) st = { txid, txStatus: 'DOUBLE_SPEND_ATTEMPTED', competingTxs: competing.length > 0 ? competing : ['00'.repeat(32)] }
    else if (this.mode === 'silent') st = { txid, txStatus: 'RECEIVED' }
    else st = { txid, txStatus: 'SEEN_ON_NETWORK' }
    this.statuses.set(txid, st)
    this.txs.set(txid, tx)
    if (st.txStatus === 'SEEN_ON_NETWORK') for (const i of tx.inputs) this.spent.set(`${i.sourceTXID}.${i.sourceOutputIndex}`, txid)
    return st
  }

  async status(txid: string): Promise<ArcadeStatus | undefined> {
    return this.statuses.get(txid)
  }

  async spender(txid: string, vout: number): Promise<string> {
    if (this.spendUnknown) throw new Error('the node answers status "NOT_FOUND"')
    return this.spent.get(`${txid}.${vout}`) ?? ''
  }

  async proof(txid: string): Promise<MerklePath | undefined> {
    return this.proofs.get(txid)
  }

  mine(txid: string): void {
    const tx = this.txs.get(txid)
    if (tx === undefined) throw new Error(`${txid} was never sent`)
    const copy = Transaction.fromEF(tx.toEF())
    this.chain.mine(copy)
    this.proofs.set(txid, copy.merklePath!)
    this.statuses.set(txid, { txid, txStatus: 'MINED' })
  }

  spendElsewhere(outpoint: string, by = 'ab'.repeat(32)): void {
    this.spent.set(outpoint, by)
  }
}

/** A lookup service whose questions are `{ kind: <class> }`, answering what it was asked. */
export class FixedLookup {
  restored = true
  answered = 0
  classify(q: LookupBody): string {
    const kind = (q.query as { kind?: unknown } | null)?.kind
    if (q.service !== Service || typeof kind !== 'string' || !Classes.some((c) => c.name === kind)) throw new Error('no such question')
    return kind
  }
  async answer(q: LookupBody): Promise<unknown> {
    this.answered++
    return { type: 'output-list', outputs: [], asked: this.classify(q) }
  }
}

export const payee = PrivateKey.fromHex('11'.repeat(32))
export const payeeKey = payee.toPublicKey().toString()

export interface Rig {
  url: string
  server: Server
  receiver: MemoryReceiver
  wallet: PayingWallet
  client: string
  chain: Chain
  front: LookupFront
  host: CountingHost
  net: TestNetwork
  gate: PaymentGate
  ls: FixedLookup
  signatures: () => number
}

export const servers: Server[] = []

export interface RigOptions {
  prices?: string
  sessions?: { max: number; ttlSeconds: number }
  budget?: BudgetConfig
  responses?: ResponseBudgetConfig
  responseBudget?: 'after-verify' | 'before-verify'
  policy?: Partial<AcceptancePolicy>
  networked?: boolean
}

/** A terms route on a free port, over a fresh chain. */
export async function rig(o: RigOptions = {}): Promise<Rig> {
  const host = countingHost()
  const chain = new Chain(11000 + nowhere * 10)
  const receiver = new MemoryReceiver()
  let signatures = 0
  const wallet = new ProtoWallet(payee)
  const sign = wallet.createSignature.bind(wallet)
  wallet.createSignature = async (args) => {
    signatures++
    return await sign(args)
  }
  const net = new TestNetwork(chain)
  const networked = o.networked ?? true
  const gate = new PaymentGate({ app: 'example', host, headers: chain.tracker, policy: { ...defaultAcceptancePolicy(), ...o.policy }, arcade: networked ? net : undefined, node: networked ? net : undefined, waitMs: 200, pollMs: 10 })
  const ls = new FixedLookup()
  const front = new LookupFront({
    app: 'example',
    service: Service,
    classes: Classes,
    paymentProtocol: Protocol,
    ls,
    host,
    prices: parsePrices(o.prices ?? 'history=5,history-after=5', Classes, 'EXAMPLE_PRICES'),
    wallet,
    receiver,
    headers: chain.tracker,
    sessions: o.sessions,
    budget: o.budget,
    responses: o.responses,
    responseBudget: o.responseBudget,
    gate,
  })
  const server = await front.listen(0, '127.0.0.1')
  servers.push(server)
  const client = PrivateKey.fromRandom()
  return {
    url: `http://127.0.0.1:${(server.address() as AddressInfo).port}`,
    server,
    receiver,
    wallet: new PayingWallet(client, chain),
    client: client.toPublicKey().toString(),
    chain,
    front,
    host,
    net,
    gate,
    ls,
    signatures: () => signatures,
  }
}

export const post = (body: unknown, headers: Record<string, string> = {}): { method: string; headers: Record<string, string>; body: string } => ({
  method: 'POST',
  headers: { 'content-type': 'application/json', ...headers },
  body: JSON.stringify(body),
})

export const ask = (kind: string): { service: string; query: { kind: string } } => ({ service: Service, query: { kind } })

/** AuthFetch prints each payment attempt; the tests read the responses instead. */
export async function quietly<T>(f: () => Promise<T>): Promise<T> {
  const saved = { warn: console.warn, info: console.info, error: console.error }
  console.warn = () => {}
  console.info = () => {}
  console.error = () => {}
  try {
    return await f()
  } finally {
    Object.assign(console, saved)
  }
}
