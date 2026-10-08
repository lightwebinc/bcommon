// Package feepolicy is where a producer's miner fee comes from: a static
// rate it was configured with, or the policy a broadcaster publishes at
// GET /v1/policy (ARC, and arcade, which answers the same shape), cached,
// bounded and degrading to the last good answer and then to the static
// rate. It does the I/O so that package mint stays pure: a Source answers
// the mint.Fees a build is given.
//
// The policy endpoint is plain HTTPS and unsigned, so its answer is held to
// guards before it is used: it must parse as whole numbers in range, a rate
// below the minimum is raised to it (a broadcast below the miners' rate is
// refused anyway), and a rate above the maximum is lowered to it, which is
// what bounds a compromised or mistaken endpoint's power to drain a wallet.
// The per-transaction ceiling, mint.Fees.Max, is refused rather than
// clamped.
//
// Live policy is opt-in. The default Source is Static at mint.DefaultFees,
// and the policy URLs follow the broadcaster a deployment settles through.
package feepolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/mint"
)

// Source answers the fees a build uses now.
type Source interface {
	Fees(ctx context.Context) (mint.Fees, error)
}

// Static is a fixed fee policy.
type Static mint.Fees

// Fees returns the configured fees, refusing a rate over zero bytes.
func (s Static) Fees(context.Context) (mint.Fees, error) {
	f := mint.Fees(s)
	if _, err := f.For(0); err != nil && !errors.Is(err, mint.ErrFeeTooHigh) {
		return mint.Fees{}, err
	}
	return f, nil
}

// Guard defaults: the network rate published today, 100 satoshis per 1000
// bytes, as both the least a live policy may lower the rate to and the most
// it may raise it to. A live policy above it is lowered to it, so a miner
// that raises its rate refuses the transaction until the operator raises
// max_rate; nothing is ever overpaid by default.
var (
	DefaultMinRate = mint.Rate{Sats: 100, Bytes: 1000}
	DefaultMaxRate = mint.Rate{Sats: 100, Bytes: 1000}
)

// Source names, as Config.Source and Status.Source spell them.
const (
	SourceStatic = "static"
	SourceARC    = "arc"
	SourceCache  = "cache"
)

// ErrConfig is a fee configuration that cannot be used.
var ErrConfig = errors.New("feepolicy: bad fee configuration")

// Config is the fee block of an application's configuration, its keys
// sorted as they are written:
//
//	"fee": {
//	  "dust": 100,
//	  "floor": 100,
//	  "max_rate": {"bytes": 1000, "satoshis": 100},
//	  "max_tx": 0,
//	  "min_rate": {"bytes": 1000, "satoshis": 100},
//	  "policy_urls": ["https://arcade.gorillapool.io"],
//	  "rate": {"bytes": 1000, "satoshis": 100},
//	  "source": "static"
//	}
//
// Every key is optional; an absent one takes the value of the defaults a
// caller passes to Source (usually mint.DefaultFees) or the guard default.
// Rate field names follow ARC's (satoshis, bytes), so a policy answer
// pastes in as a rate.
type Config struct {
	Dust       *uint64    `json:"dust,omitempty"`
	Floor      *uint64    `json:"floor,omitempty"`
	MaxRate    *mint.Rate `json:"max_rate,omitempty"`
	MaxTx      uint64     `json:"max_tx,omitempty"`
	MinRate    *mint.Rate `json:"min_rate,omitempty"`
	PolicyURLs []string   `json:"policy_urls,omitempty"`
	Rate       *mint.Rate `json:"rate,omitempty"`
	// Source is "static" (the default) or "arc" (also spelled "arcade"):
	// the policy of PolicyURLs, live.
	Source string `json:"source,omitempty"`
}

// MergeLegacy folds the older keys fee_sat_per_byte and fee_floor into c,
// which they alias: a rate {fee_sat_per_byte, 1} and a floor. Either old
// key beside the new key it aliases is ambiguous and refused, as is a
// zero fee_sat_per_byte. nil pointers are absent keys. c may be nil.
func MergeLegacy(c *Config, satPerByte, floor *uint64) (*Config, error) {
	out := Config{}
	if c != nil {
		out = *c
	}
	if satPerByte != nil {
		if out.Rate != nil {
			return nil, fmt.Errorf("%w: fee_sat_per_byte and fee.rate are both set; keep one", ErrConfig)
		}
		if *satPerByte == 0 {
			return nil, fmt.Errorf("%w: fee_sat_per_byte is 0", ErrConfig)
		}
		out.Rate = &mint.Rate{Sats: *satPerByte, Bytes: 1}
	}
	if floor != nil {
		if out.Floor != nil {
			return nil, fmt.Errorf("%w: fee_floor and fee.floor are both set; keep one", ErrConfig)
		}
		out.Floor = floor
	}
	return &out, nil
}

// Fees is the static fees the configuration describes over defaults.
func (c *Config) Fees(defaults mint.Fees) (mint.Fees, error) {
	f := defaults
	if c == nil {
		return f, nil
	}
	if c.Rate != nil {
		if c.Rate.Bytes == 0 || c.Rate.Sats == 0 {
			return mint.Fees{}, fmt.Errorf("%w: rate %s needs satoshis and bytes of at least 1", ErrConfig, c.Rate)
		}
		f.Rate, f.SatPerByte = *c.Rate, 0
	}
	if c.Floor != nil {
		f.Floor = *c.Floor
	}
	if c.Dust != nil {
		f.Dust = *c.Dust
	}
	if c.MaxTx != 0 {
		f.Max = c.MaxTx
	}
	if c.MaxRate != nil {
		if c.MaxRate.Bytes == 0 {
			return mint.Fees{}, fmt.Errorf("%w: max_rate %s is over zero bytes", ErrConfig, c.MaxRate)
		}
		f.MaxRate = *c.MaxRate
	}
	if _, err := f.For(0); err != nil && !errors.Is(err, mint.ErrFeeTooHigh) {
		return mint.Fees{}, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if f.Max > 0 && f.Floor > f.Max {
		return mint.Fees{}, fmt.Errorf("%w: floor %d is above max_tx %d", ErrConfig, f.Floor, f.Max)
	}
	return f, nil
}

// Build builds the configured Source over defaults: Static, or an ARC
// policy source whose fallback is the static fees.
func (c *Config) Build(defaults mint.Fees) (Source, error) {
	f, err := c.Fees(defaults)
	if err != nil {
		return nil, err
	}
	src := ""
	if c != nil {
		src = c.Source
	}
	switch src {
	case "", SourceStatic:
		return Static(f), nil
	case SourceARC, "arcade":
	default:
		return nil, fmt.Errorf("%w: source %q is static or arc", ErrConfig, src)
	}
	if len(c.PolicyURLs) == 0 {
		return nil, fmt.Errorf("%w: source %s needs policy_urls", ErrConfig, src)
	}
	a := &ARC{URLs: c.PolicyURLs, Base: f, Min: DefaultMinRate, Max: DefaultMaxRate}
	if c.MinRate != nil {
		a.Min = *c.MinRate
	}
	if c.MaxRate != nil {
		a.Max = *c.MaxRate
	}
	if a.Max.Bytes == 0 || a.Min.Bytes == 0 || a.Min.Cmp(a.Max) > 0 {
		return nil, fmt.Errorf("%w: min_rate %s and max_rate %s", ErrConfig, a.Min, a.Max)
	}
	for _, u := range a.URLs {
		if _, err := policyURL(u); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// ParseConfig reads a fee block's JSON, refusing unknown keys.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return &c, nil
}
