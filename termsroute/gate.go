package termsroute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/acceptance"
	bmodule "github.com/lightwebinc/bcommon/module"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
)

// GateOptions builds a Gate.
type GateOptions struct {
	// App is the metric prefix.
	App  string
	Host bmodule.Host
	// Headers are the host's block headers, which a payment verifies against.
	Headers chaintracker.ChainTracker
	Policy  acceptance.Policy
	// Arcade is the broadcaster the host collects through; Asset the node's
	// asset service (spend view, proofs). Without both no payment is fast.
	Arcade *publish.Arcade
	Asset  *nodeapi.Asset
	// Node, when Asset is nil, is another spend view and proof source in
	// its place, such as WhatsOnChain (nodeapi.WoC) for a host with no
	// node.
	Node NodeView
	// MaxAge is how long a fast payment may stay unmined before it is
	// reported; a day when zero. MaxWatched is the most fast payments
	// watched at once, 100000 when zero; past it a payment is held.
	MaxAge     time.Duration
	MaxWatched int
	// StateDir is where unsettleable.jsonl is written; none when empty.
	StateDir string
	Now      func() time.Time
}

// NodeView is a node's view a gate reads: who spent an output, and a mined
// transaction's proof. *nodeapi.Asset and *nodeapi.WoC are both one.
type NodeView interface {
	acceptance.SpendView
	acceptance.ProofSource
}

var (
	_ NodeView = (*nodeapi.Asset)(nil)
	_ NodeView = (*nodeapi.WoC)(nil)
)

// UnsettleableFile records each fast payment the watch found lost.
const UnsettleableFile = "unsettleable.jsonl"

// PaymentLabels are the decision and reason pairs the route counts
// payments under, preset so a dashboard sees zeros.
var PaymentLabels = [][2]string{
	{"requested", "no-payment"},
	{"fast", "at-or-below-threshold"},
	{"fast", "mined"},
	{"hold", "above-threshold"},
	{"hold", "payer-limit"},
	{"hold", "total-limit"},
	{"hold", "payer-flagged"},
	{"hold", "no-broadcast-leg"},
	{"hold", "broadcast-unconfirmed"},
	{"hold", "no-network-verdict"},
	{"hold", "double-spend-attempted"},
	{"hold", "spend-view-unknown"},
	{"hold", "already-charged"},
	{"hold", "payer-asked-hold"},
	{"hold", "price-unknown"},
	{"hold", "price-stale"},
	{"hold", "no-txid"},
	{"hold", "watch-limit"},
	{"refuse", "pays-nothing"},
	{"refuse", "malformed"},
	{"refuse", "wrong-script"},
	{"refuse", "spv-failed"},
	{"refuse", "not-final"},
	{"refuse", "outputs-exceed-inputs"},
	{"refuse", "network-refused"},
	{"refuse", "double-spent"},
	{"refuse", "replayed"},
	{"refuse", "conflict"},
}

// EventKinds are what became of a fast payment, as the route counts them.
var EventKinds = []string{"confirmed", "double-spent", "refused", "unmined"}

// GateVerdict is a decision on one payment: "fast", "hold" or "refuse",
// the reason, and the reason in words.
type GateVerdict struct {
	Decision string
	Reason   string
	Detail   string
}

// Gate is the fast path, the hold, and the watch over what the fast path
// took: bcommon's acceptance rule, counted and recorded as the TypeScript
// route counts and records it.
type Gate struct {
	o        GateOptions
	verifier *acceptance.Verifier
	monitor  *acceptance.Monitor
	now      func() time.Time
	mu       sync.Mutex
	sweeping bool
}

// NewGate makes a gate. Every payment is held when Arcade is nil, or both
// Asset and Node are.
func NewGate(o GateOptions) *Gate {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxAge <= 0 {
		o.MaxAge = 24 * time.Hour
	}
	if o.MaxWatched <= 0 {
		o.MaxWatched = 100_000
	}
	g := &Gate{o: o, now: o.Now}
	v := &acceptance.Verifier{Policy: o.Policy, Exposure: acceptance.NewExposure(), Headers: o.Headers}
	if o.Arcade != nil {
		v.Settler = o.Arcade
		v.Status = []acceptance.StatusSource{o.Arcade}
	}
	if o.Asset != nil {
		v.Spends = o.Asset
		v.Proofs = o.Asset
	} else if o.Node != nil {
		v.Spends = o.Node
		v.Proofs = o.Node
	}
	g.verifier = v
	g.monitor = &acceptance.Monitor{Verifier: v, MaxAge: o.MaxAge, Hook: g.record}
	for _, l := range PaymentLabels {
		o.Host.Metrics().Preset(o.App+"_payments_total", map[string]string{"decision": l[0], "reason": l[1]})
	}
	for _, k := range EventKinds {
		o.Host.Metrics().Preset(o.App+"_payment_events_total", map[string]string{"kind": k})
	}
	o.Host.Metrics().Gauge(o.App+"_payments_unmined", func() float64 { return float64(len(g.monitor.Watching())) })
	g.reflag()
	return g
}

// reflag flags again every payer unsettleable.jsonl names: a flag outlives
// a restart.
func (g *Gate) reflag() {
	if g.o.StateDir == "" {
		return
	}
	text, err := os.ReadFile(filepath.Join(g.o.StateDir, UnsettleableFile))
	if err != nil {
		return
	}
	for _, l := range strings.Split(string(text), "\n") {
		var v struct {
			Kind  string `json:"kind"`
			Txid  string `json:"txid"`
			Payer string `json:"payer"`
		}
		if json.Unmarshal([]byte(l), &v) == nil && v.Payer != "" && v.Kind != "" && v.Txid != "" {
			g.verifier.Exposure.Flag(v.Payer, v.Kind+": "+v.Txid)
		}
	}
}

// Networked is whether the host can take a payment on the fast path at all.
func (g *Gate) Networked() bool { return g.o.Arcade != nil && g.node() != nil }

// node is the node view the gate reads: Asset, else Node.
func (g *Gate) node() NodeView {
	if g.o.Asset != nil {
		return g.o.Asset
	}
	return g.o.Node
}

// Count counts one decision.
func (g *Gate) Count(decision, reason string) {
	g.o.Host.Metrics().Inc(g.o.App+"_payments_total", map[string]string{"decision": decision, "reason": reason}, 1)
}

// Forget releases and stops watching a payment the route did not take
// after all.
func (g *Gate) Forget(txid string) {
	g.verifier.Exposure.Release(txid)
}

// Accept decides tx, which pays sats to output 0 locked by script and
// passed the route's own checks, for payer.
func (g *Gate) Accept(ctx context.Context, tx *transaction.Transaction, payer string, script []byte, sats uint64) GateVerdict {
	v := g.decide(ctx, tx, payer, script, sats)
	g.Count(v.Decision, v.Reason)
	return v
}

func (g *Gate) decide(ctx context.Context, tx *transaction.Transaction, payer string, script []byte, sats uint64) GateVerdict {
	if len(g.monitor.Watching()) >= g.o.MaxWatched && tx.MerklePath == nil {
		if g.mined(ctx, tx) {
			return GateVerdict{"fast", string(acceptance.ReasonMined), ""}
		}
		return GateVerdict{"hold", "watch-limit", "too many fast payments are waiting to mine"}
	}
	vd, err := g.verifier.Accept(ctx, acceptance.Payment{Tx: tx, Payer: payer, Pays: []acceptance.Output{{Vout: 0, Script: script, Sats: sats}}})
	if err != nil {
		return GateVerdict{"hold", string(acceptance.ReasonBroadcastUnknown), err.Error()}
	}
	out := GateVerdict{Reason: string(vd.Reason), Detail: vd.Detail}
	switch vd.Decision {
	case acceptance.Refuse:
		out.Decision = "refuse"
	case acceptance.Fast:
		out.Decision = "fast"
		if vd.Reason != acceptance.ReasonMined && !g.Networked() {
			// Not fast without both arcade and the node: the payment is
			// still broadcast where it can be, and still answered mined.
			g.verifier.Exposure.Release(tx.TxID().String())
			if g.mined(ctx, tx) {
				out.Reason = string(acceptance.ReasonMined)
				break
			}
			out = GateVerdict{"hold", string(acceptance.ReasonNoBroadcast), "this host has no arcade or no node to take a payment fast"}
		}
	default:
		out.Decision = "hold"
		if g.mined(ctx, tx) {
			out = GateVerdict{"fast", string(acceptance.ReasonMined), ""}
		}
	}
	if out.Decision == "fast" && out.Reason != string(acceptance.ReasonMined) {
		g.monitor.Watch(tx, payer, sats)
	}
	return out
}

// mined is whether the node serves a proof of tx that verifies against the
// host's headers.
func (g *Gate) mined(ctx context.Context, tx *transaction.Transaction) bool {
	n := g.node()
	if n == nil || g.o.Headers == nil {
		return false
	}
	txid := tx.TxID()
	mp, _, err := n.Proof(ctx, txid.String())
	if err != nil || mp == nil {
		return false
	}
	ok, err := mp.Verify(ctx, txid, g.o.Headers)
	return err == nil && ok
}

// Restore watches a fast payment taken before a restart, charged as it was.
func (g *Gate) Restore(tx *transaction.Transaction, payer string, sats uint64, at time.Time) {
	age := g.now().Sub(at)
	if age > g.o.MaxAge {
		return
	}
	if w := g.o.Policy.Window; w <= 0 || age <= w {
		g.verifier.Exposure.Charge(payer, tx.TxID().String(), sats, at)
	}
	g.monitor.Watch(tx, payer, sats)
}

// Watching is the txids being watched.
func (g *Gate) Watching() []string { return g.monitor.Watching() }

// Sweep is one pass over the fast payments not yet mined. A pass already
// running is left to finish.
func (g *Gate) Sweep(ctx context.Context) {
	g.mu.Lock()
	if g.sweeping {
		g.mu.Unlock()
		return
	}
	g.sweeping = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.sweeping = false
		g.mu.Unlock()
	}()
	g.monitor.Sweep(ctx)
}

func (g *Gate) record(ev acceptance.Event) {
	kind := map[acceptance.EventKind]string{acceptance.Confirmed: "confirmed", acceptance.DoubleSpent: "double-spent", acceptance.Refused: "refused", acceptance.Unmined: "unmined"}[ev.Kind]
	g.o.Host.Metrics().Inc(g.o.App+"_payment_events_total", map[string]string{"kind": kind}, 1)
	if ev.Kind == acceptance.Confirmed {
		return
	}
	g.o.Host.Log(g.o.App+" fast payment lost; its payer is held for confirmation", map[string]any{"kind": kind, "txid": ev.Txid, "payer": ev.Payer, "sats": ev.Sats, "why": ev.Why})
	if g.o.StateDir == "" {
		return
	}
	line, _ := json.Marshal(map[string]any{"kind": kind, "txid": ev.Txid, "payer": ev.Payer, "sats": ev.Sats, "why": ev.Why, "at": g.now().Unix()})
	if err := os.MkdirAll(g.o.StateDir, 0o700); err == nil {
		f, err := os.OpenFile(filepath.Join(g.o.StateDir, UnsettleableFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
			return
		}
	}
	g.o.Host.Log(g.o.App+" unsettleable record failed", map[string]any{"txid": ev.Txid})
}

// AcceptConfig is the acceptance settings a host reads from its environment.
type AcceptConfig struct {
	ArcadeURL string
	AssetURL  string
	// Chain is WhatsOnChain's network ("main" or "test") as the node view,
	// for a host with no node (<P>_CHAIN=woc:main or woc:test); WocKey its
	// API key (<P>_WOC_KEY), if any.
	Chain  string
	WocKey string
	Policy acceptance.Policy
}

var durationRE = regexp.MustCompile(`^([0-9]+)(ms|s|m|h)$`)

// ParseDuration parses <n>ms, <n>s, <n>m or <n>h; name is the setting, for
// the error.
func ParseDuration(name, raw string) (time.Duration, error) {
	m := durationRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return 0, fmt.Errorf("%s: %q is not a duration like 2s, 10m or 1h", name, raw)
	}
	n, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil || n > MaxSafe {
		return 0, fmt.Errorf("%s: %q is not a duration like 2s, 10m or 1h", name, raw)
	}
	unit := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
}

// ParseAcceptConfig reads <P>_ARCADE_URL, <P>_ASSET_URL (or <P>_CHAIN,
// woc:main or woc:test, with <P>_WOC_KEY, for a host with no node) and
// <P>_ACCEPT_THRESHOLD_SATS, _ACCEPT_PAYER_LIMIT, _ACCEPT_TOTAL_LIMIT,
// _ACCEPT_WINDOW and _ACCEPT_WATCH through getenv, as the TypeScript
// package's parseAcceptConfig reads them. Defaults are bcommon's
// DefaultPolicy and no watch.
func ParseAcceptConfig(prefix string, getenv func(string) string) (AcceptConfig, error) {
	policy := acceptance.DefaultPolicy()
	sats := func(key string, def uint64) (uint64, error) {
		raw := strings.TrimSpace(getenv(prefix + "_" + key))
		if raw == "" {
			return def, nil
		}
		n, err := strconv.ParseUint(raw, 10, 64)
		if !decimal.MatchString(raw) || err != nil || n > MaxSafe {
			return 0, fmt.Errorf("%s_%s: %q is not a whole number of satoshis", prefix, key, raw)
		}
		return n, nil
	}
	dur := func(key string, def time.Duration) (time.Duration, error) {
		raw := strings.TrimSpace(getenv(prefix + "_" + key))
		if raw == "" {
			return def, nil
		}
		return ParseDuration(prefix+"_"+key, raw)
	}
	var err error
	if policy.ThresholdSats, err = sats("ACCEPT_THRESHOLD_SATS", policy.ThresholdSats); err != nil {
		return AcceptConfig{}, err
	}
	if policy.PayerLimit, err = sats("ACCEPT_PAYER_LIMIT", policy.ThresholdSats); err != nil {
		return AcceptConfig{}, err
	}
	if policy.TotalLimit, err = sats("ACCEPT_TOTAL_LIMIT", 10*policy.ThresholdSats); err != nil {
		return AcceptConfig{}, err
	}
	if policy.Window, err = dur("ACCEPT_WINDOW", policy.Window); err != nil {
		return AcceptConfig{}, err
	}
	if policy.Watch, err = dur("ACCEPT_WATCH", 0); err != nil {
		return AcceptConfig{}, err
	}
	url := func(key string) (string, error) {
		raw := strings.TrimSpace(getenv(prefix + "_" + key))
		if raw == "" {
			return "", nil
		}
		if !(strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")) || strings.ContainsAny(raw, " \t") {
			return "", fmt.Errorf("%s_%s: %q is not an http(s) URL", prefix, key, raw)
		}
		return raw, nil
	}
	c := AcceptConfig{Policy: policy}
	if c.ArcadeURL, err = url("ARCADE_URL"); err != nil {
		return AcceptConfig{}, err
	}
	if c.AssetURL, err = url("ASSET_URL"); err != nil {
		return AcceptConfig{}, err
	}
	if chain := strings.TrimSpace(getenv(prefix + "_CHAIN")); chain != "" {
		if chain != "woc:main" && chain != "woc:test" {
			return AcceptConfig{}, fmt.Errorf("%s_CHAIN: %q is woc:main or woc:test", prefix, chain)
		}
		if c.AssetURL != "" {
			return AcceptConfig{}, fmt.Errorf("%s_CHAIN and %s_ASSET_URL are both set; keep one", prefix, prefix)
		}
		c.Chain = strings.TrimPrefix(chain, "woc:")
		c.WocKey = strings.TrimSpace(getenv(prefix + "_WOC_KEY"))
	}
	return c, nil
}

// GateFor is the gate for a host's configuration.
func GateFor(app string, host bmodule.Host, headers chaintracker.ChainTracker, c AcceptConfig, stateDir string) *Gate {
	o := GateOptions{App: app, Host: host, Headers: headers, Policy: c.Policy, StateDir: stateDir}
	client := &http.Client{Timeout: 30 * time.Second}
	if c.ArcadeURL != "" {
		o.Arcade = &publish.Arcade{Base: c.ArcadeURL, Client: client}
	}
	if c.AssetURL != "" {
		o.Asset = &nodeapi.Asset{Base: c.AssetURL, Client: client}
		if o.Arcade != nil {
			o.Arcade.Asset = o.Asset
		}
	} else if c.Chain != "" {
		if woc, err := nodeapi.NewWoC(c.Chain, c.WocKey); err == nil {
			o.Node = woc
			if o.Arcade != nil {
				o.Arcade.Spends = woc
			}
		}
	}
	return NewGate(o)
}

var errNoGate = errors.New("termsroute: no payment gate")

func defaultPolicy() acceptance.Policy { return acceptance.DefaultPolicy() }
