package bwallet

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// The methods in this file complete wallet.Interface over ProtoWallet.
//
// The rule, held by TestEveryInterfaceMethodAnswers: no method returns
// nil, nil. A method that is not implemented says so with ErrNotSupported and
// its own name, because "not supported" and "succeeded with nothing" are
// different answers and CompletedProtoWallet's habit of giving the second for
// the first is how a caller nil-derefs in production.

func notSupported(method string) error {
	return fmt.Errorf("%w: %s", ErrNotSupported, method)
}

// ListOutputs answers from the pool. Only the profile's FundBasket (or an
// empty basket, meaning all) holds anything; any other basket is
// legitimately empty.
//
// Spendable is decided by coinbase maturity against Chain's tip when a Chain
// is configured. Without one a coinbase's maturity is unknowable, and it is
// reported NOT spendable rather than guessed: a wrong "spendable" is a
// rejected transaction, a wrong "not yet" is a wait.
func (e *Embedded) ListOutputs(ctx context.Context, args wallet.ListOutputsArgs, _ string) (*wallet.ListOutputsResult, error) {
	if err := e.profile.Validate(); err != nil {
		return nil, err
	}
	res := &wallet.ListOutputsResult{Outputs: []wallet.Output{}}
	if args.Basket != "" && args.Basket != e.profile.FundBasket {
		return res, nil
	}
	held := e.Pool.Outputs()
	res.TotalOutputs = uint32(len(held))

	var tip uint32
	haveTip := false
	if e.Chain != nil {
		h, err := e.Chain.CurrentHeight(ctx)
		if err != nil {
			return nil, fmt.Errorf("bwallet: ListOutputs: tip: %w", err)
		}
		tip, haveTip = h, true
	}

	// Offset and limit are converted to int only below len(held). On a
	// 32-bit target int() of a value from 1<<31 up is negative: an offset
	// would then index before the first output, and a limit would list none.
	offset := 0
	if args.Offset != nil {
		offset = len(held)
		if uint64(*args.Offset) < uint64(len(held)) {
			offset = int(*args.Offset)
		}
	}
	limit := len(held)
	if args.Limit != nil && *args.Limit > 0 && uint64(*args.Limit) < uint64(len(held)) {
		limit = int(*args.Limit)
	}
	for i := offset; i < len(held) && len(res.Outputs) < limit; i++ {
		o := held[i]
		txid, err := chainhash.NewHashFromHex(o.TxID)
		if err != nil {
			return nil, fmt.Errorf("bwallet: output %s: %w", o.Outpoint(), err)
		}
		ls, err := hex.DecodeString(o.LockingScript)
		if err != nil {
			return nil, fmt.Errorf("bwallet: output %s script: %w", o.Outpoint(), err)
		}
		spendable := !o.Coinbase
		if haveTip {
			spendable = o.Spendable(tip)
		}
		tags := []string{}
		if o.Coinbase {
			tags = append(tags, "coinbase")
		}
		res.Outputs = append(res.Outputs, wallet.Output{
			Satoshis:      o.Satoshis,
			LockingScript: ls,
			Spendable:     spendable,
			Tags:          tags,
			Outpoint:      transaction.Outpoint{Txid: *txid, Index: o.Vout},
		})
	}
	return res, nil
}

// GetHeight answers from Chain, or ErrNoChain.
func (e *Embedded) GetHeight(ctx context.Context, _ any, _ string) (*wallet.GetHeightResult, error) {
	if e.Chain == nil {
		return nil, ErrNoChain
	}
	h, err := e.Chain.CurrentHeight(ctx)
	if err != nil {
		return nil, err
	}
	return &wallet.GetHeightResult{Height: h}, nil
}

// GetHeaderForHeight answers from Chain when it also serves headers, or
// ErrNoChain, or ErrNotSupported when the chain source serves heights only.
func (e *Embedded) GetHeaderForHeight(ctx context.Context, args wallet.GetHeaderArgs, _ string) (*wallet.GetHeaderResult, error) {
	if e.Chain == nil {
		return nil, ErrNoChain
	}
	hs, ok := e.Chain.(HeaderSource)
	if !ok {
		return nil, notSupported("GetHeaderForHeight (chain source serves heights only)")
	}
	raw, err := hs.HeaderForHeight(ctx, args.Height)
	if err != nil {
		return nil, err
	}
	return &wallet.GetHeaderResult{Header: raw}, nil
}

// GetNetwork is testnet unless configured mainnet, so a private chain
// answers testnet too.
func (e *Embedded) GetNetwork(context.Context, any, string) (*wallet.GetNetworkResult, error) {
	if e.Mainnet {
		return &wallet.GetNetworkResult{Network: wallet.NetworkMainnet}, nil
	}
	return &wallet.GetNetworkResult{Network: wallet.NetworkTestnet}, nil
}

// GetVersion names this backend: the profile's Version.
func (e *Embedded) GetVersion(context.Context, any, string) (*wallet.GetVersionResult, error) {
	if err := e.profile.Validate(); err != nil {
		return nil, err
	}
	return &wallet.GetVersionResult{Version: e.profile.Version}, nil
}

// IsAuthenticated is always true: the key is on disk, there is no session.
func (e *Embedded) IsAuthenticated(context.Context, any, string) (*wallet.AuthenticatedResult, error) {
	return &wallet.AuthenticatedResult{Authenticated: true}, nil
}

// WaitForAuthentication has nothing to wait for and says so rather than
// answering true: a caller that waits expects a session to appear.
func (e *Embedded) WaitForAuthentication(context.Context, any, string) (*wallet.AuthenticatedResult, error) {
	return nil, notSupported("WaitForAuthentication")
}

// The BRC-100 action flow is not built yet. Until it is, the
// raw builder with FundUnlocker is the way to spend, and these say so.

func (e *Embedded) CreateAction(context.Context, wallet.CreateActionArgs, string) (*wallet.CreateActionResult, error) {
	return nil, notSupported("CreateAction")
}

func (e *Embedded) SignAction(context.Context, wallet.SignActionArgs, string) (*wallet.SignActionResult, error) {
	return nil, notSupported("SignAction")
}

func (e *Embedded) AbortAction(context.Context, wallet.AbortActionArgs, string) (*wallet.AbortActionResult, error) {
	return nil, notSupported("AbortAction")
}

func (e *Embedded) ListActions(context.Context, wallet.ListActionsArgs, string) (*wallet.ListActionsResult, error) {
	return nil, notSupported("ListActions")
}

func (e *Embedded) InternalizeAction(context.Context, wallet.InternalizeActionArgs, string) (*wallet.InternalizeActionResult, error) {
	return nil, notSupported("InternalizeAction")
}

func (e *Embedded) RelinquishOutput(context.Context, wallet.RelinquishOutputArgs, string) (*wallet.RelinquishOutputResult, error) {
	return nil, notSupported("RelinquishOutput")
}

func (e *Embedded) AcquireCertificate(context.Context, wallet.AcquireCertificateArgs, string) (*wallet.Certificate, error) {
	return nil, notSupported("AcquireCertificate")
}

func (e *Embedded) ListCertificates(context.Context, wallet.ListCertificatesArgs, string) (*wallet.ListCertificatesResult, error) {
	return nil, notSupported("ListCertificates")
}

func (e *Embedded) ProveCertificate(context.Context, wallet.ProveCertificateArgs, string) (*wallet.ProveCertificateResult, error) {
	return nil, notSupported("ProveCertificate")
}

func (e *Embedded) RelinquishCertificate(context.Context, wallet.RelinquishCertificateArgs, string) (*wallet.RelinquishCertificateResult, error) {
	return nil, notSupported("RelinquishCertificate")
}

func (e *Embedded) RevealCounterpartyKeyLinkage(context.Context, wallet.RevealCounterpartyKeyLinkageArgs, string) (*wallet.RevealCounterpartyKeyLinkageResult, error) {
	return nil, notSupported("RevealCounterpartyKeyLinkage")
}

func (e *Embedded) RevealSpecificKeyLinkage(context.Context, wallet.RevealSpecificKeyLinkageArgs, string) (*wallet.RevealSpecificKeyLinkageResult, error) {
	return nil, notSupported("RevealSpecificKeyLinkage")
}

func (e *Embedded) DiscoverByIdentityKey(context.Context, wallet.DiscoverByIdentityKeyArgs, string) (*wallet.DiscoverCertificatesResult, error) {
	return nil, notSupported("DiscoverByIdentityKey")
}

func (e *Embedded) DiscoverByAttributes(context.Context, wallet.DiscoverByAttributesArgs, string) (*wallet.DiscoverCertificatesResult, error) {
	return nil, notSupported("DiscoverByAttributes")
}
