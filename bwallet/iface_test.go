package bwallet

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/wallet"
)

type fakeChain struct{ tip uint32 }

func (f fakeChain) CurrentHeight(context.Context) (uint32, error) { return f.tip, nil }

type fakeChainWithHeaders struct{ fakeChain }

func (f fakeChainWithHeaders) HeaderForHeight(_ context.Context, h uint32) ([]byte, error) {
	return []byte{byte(h)}, nil
}

// The count is run, not counted by eye.
func TestInterfaceHas28Methods(t *testing.T) {
	if n := reflect.TypeFor[wallet.Interface]().NumMethod(); n != 28 {
		t.Fatalf("wallet.Interface has %d methods at this SDK pin, the tests below assume 28", n)
	}
}

// Every method, called with zero arguments, answers a result or an error and
// never nil, nil. This is the whole reason the embedded backend exists
// instead of CompletedProtoWallet.
func TestEveryInterfaceMethodAnswers(t *testing.T) {
	e := newWallet(t)
	iface := reflect.TypeFor[wallet.Interface]()
	val := reflect.ValueOf(e)
	for i := 0; i < iface.NumMethod(); i++ {
		m := iface.Method(i)
		fn := val.MethodByName(m.Name)
		if !fn.IsValid() {
			t.Fatalf("%s: not found on *Embedded", m.Name)
		}
		out := fn.Call([]reflect.Value{
			reflect.ValueOf(context.Background()),
			reflect.Zero(m.Type.In(1)),
			reflect.ValueOf(""),
		})
		if out[0].IsNil() && out[1].IsNil() {
			t.Errorf("%s returned nil, nil", m.Name)
		}
	}
}

func TestUnsupportedMethodsNameThemselves(t *testing.T) {
	e := newWallet(t)
	ctx := context.Background()
	cases := map[string]func() error{
		"CreateAction":       func() error { _, err := e.CreateAction(ctx, wallet.CreateActionArgs{}, ""); return err },
		"SignAction":         func() error { _, err := e.SignAction(ctx, wallet.SignActionArgs{}, ""); return err },
		"AbortAction":        func() error { _, err := e.AbortAction(ctx, wallet.AbortActionArgs{}, ""); return err },
		"ListActions":        func() error { _, err := e.ListActions(ctx, wallet.ListActionsArgs{}, ""); return err },
		"InternalizeAction":  func() error { _, err := e.InternalizeAction(ctx, wallet.InternalizeActionArgs{}, ""); return err },
		"RelinquishOutput":   func() error { _, err := e.RelinquishOutput(ctx, wallet.RelinquishOutputArgs{}, ""); return err },
		"AcquireCertificate": func() error { _, err := e.AcquireCertificate(ctx, wallet.AcquireCertificateArgs{}, ""); return err },
		"ListCertificates":   func() error { _, err := e.ListCertificates(ctx, wallet.ListCertificatesArgs{}, ""); return err },
		"ProveCertificate":   func() error { _, err := e.ProveCertificate(ctx, wallet.ProveCertificateArgs{}, ""); return err },
		"RelinquishCertificate": func() error {
			_, err := e.RelinquishCertificate(ctx, wallet.RelinquishCertificateArgs{}, "")
			return err
		},
		"RevealCounterpartyKeyLinkage": func() error {
			_, err := e.RevealCounterpartyKeyLinkage(ctx, wallet.RevealCounterpartyKeyLinkageArgs{}, "")
			return err
		},
		"RevealSpecificKeyLinkage": func() error {
			_, err := e.RevealSpecificKeyLinkage(ctx, wallet.RevealSpecificKeyLinkageArgs{}, "")
			return err
		},
		"DiscoverByIdentityKey": func() error {
			_, err := e.DiscoverByIdentityKey(ctx, wallet.DiscoverByIdentityKeyArgs{}, "")
			return err
		},
		"DiscoverByAttributes":  func() error { _, err := e.DiscoverByAttributes(ctx, wallet.DiscoverByAttributesArgs{}, ""); return err },
		"WaitForAuthentication": func() error { _, err := e.WaitForAuthentication(ctx, nil, ""); return err },
	}
	for name, call := range cases {
		err := call()
		if !errors.Is(err, ErrNotSupported) {
			t.Errorf("%s: err %v, want ErrNotSupported", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), name) {
			t.Errorf("%s: error %q does not name the method", name, err)
		}
	}
}

func TestListOutputsAnswersFromThePool(t *testing.T) {
	e := newWallet(t)
	cb := out(0xaa, 0, 5000, 10, true)
	ch := out(0xbb, 2, 700, 50, false)
	if _, err := e.Pool.Add(cb, ch); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	res, err := e.ListOutputs(ctx, wallet.ListOutputsArgs{Basket: testProfile.FundBasket}, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalOutputs != 2 || len(res.Outputs) != 2 {
		t.Fatalf("total %d, listed %d", res.TotalOutputs, len(res.Outputs))
	}
	if got := res.Outputs[0].Outpoint.String(); got != cb.Outpoint() {
		t.Fatalf("outpoint %q, want %q", got, cb.Outpoint())
	}
	if res.Outputs[0].Satoshis != 5000 || len(res.Outputs[0].LockingScript) != 25 {
		t.Fatalf("output 0: %+v", res.Outputs[0])
	}
	// No chain: a coinbase's maturity is unknowable and is not guessed.
	if res.Outputs[0].Spendable || !res.Outputs[1].Spendable {
		t.Fatalf("spendable flags without a chain: %v %v, want false true", res.Outputs[0].Spendable, res.Outputs[1].Spendable)
	}

	e.Chain = fakeChain{tip: 200}
	res, err = e.ListOutputs(ctx, wallet.ListOutputsArgs{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Outputs[0].Spendable {
		t.Fatal("coinbase at 10 is mature at tip 200")
	}

	one := uint32(1)
	res, err = e.ListOutputs(ctx, wallet.ListOutputsArgs{Limit: &one, Offset: &one}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outputs) != 1 || res.Outputs[0].Outpoint.String() != ch.Outpoint() {
		t.Fatalf("limit 1 offset 1: %+v", res.Outputs)
	}

	// 1<<31 is negative as a 32-bit int. As an offset it is past the end, so
	// nothing is listed; as a limit it is above the count, so everything is.
	wrap := uint32(1 << 31)
	res, err = e.ListOutputs(ctx, wallet.ListOutputsArgs{Offset: &wrap}, "")
	if err != nil || len(res.Outputs) != 0 || res.TotalOutputs != 2 {
		t.Fatalf("offset 1<<31: %d outputs of %d, err %v; want none of 2", len(res.Outputs), res.TotalOutputs, err)
	}
	res, err = e.ListOutputs(ctx, wallet.ListOutputsArgs{Limit: &wrap}, "")
	if err != nil || len(res.Outputs) != 2 {
		t.Fatalf("limit 1<<31: %d outputs, err %v; want 2", len(res.Outputs), err)
	}

	res, err = e.ListOutputs(ctx, wallet.ListOutputsArgs{Basket: "somebody-elses"}, "")
	if err != nil || len(res.Outputs) != 0 {
		t.Fatalf("another basket: %d outputs err %v, want an empty answer", len(res.Outputs), err)
	}
}

func TestHeightAndHeaderComeFromTheChainOrRefuse(t *testing.T) {
	e := newWallet(t)
	ctx := context.Background()
	if _, err := e.GetHeight(ctx, nil, ""); !errors.Is(err, ErrNoChain) {
		t.Fatalf("GetHeight without a chain: %v, want ErrNoChain", err)
	}
	if _, err := e.GetHeaderForHeight(ctx, wallet.GetHeaderArgs{Height: 1}, ""); !errors.Is(err, ErrNoChain) {
		t.Fatalf("GetHeaderForHeight without a chain: %v, want ErrNoChain", err)
	}
	e.Chain = fakeChain{tip: 4242}
	h, err := e.GetHeight(ctx, nil, "")
	if err != nil || h.Height != 4242 {
		t.Fatalf("GetHeight: %v %v", h, err)
	}
	if _, err := e.GetHeaderForHeight(ctx, wallet.GetHeaderArgs{Height: 1}, ""); !errors.Is(err, ErrNotSupported) {
		t.Fatalf("a heights-only chain: %v, want ErrNotSupported", err)
	}
	e.Chain = fakeChainWithHeaders{fakeChain{tip: 9}}
	hdr, err := e.GetHeaderForHeight(ctx, wallet.GetHeaderArgs{Height: 7}, "")
	if err != nil || len(hdr.Header) != 1 || hdr.Header[0] != 7 {
		t.Fatalf("GetHeaderForHeight: %v %v", hdr, err)
	}
}

func TestNetworkVersionAndAuthentication(t *testing.T) {
	e := newWallet(t)
	ctx := context.Background()
	n, err := e.GetNetwork(ctx, nil, "")
	if err != nil || n.Network != wallet.NetworkTestnet {
		t.Fatalf("GetNetwork: %v %v", n, err)
	}
	e.Mainnet = true
	if n, _ := e.GetNetwork(ctx, nil, ""); n.Network != wallet.NetworkMainnet {
		t.Fatal("Mainnet flag not reflected")
	}
	v, err := e.GetVersion(ctx, nil, "")
	if err != nil || v.Version != testProfile.Version {
		t.Fatalf("GetVersion: %v %v", v, err)
	}
	a, err := e.IsAuthenticated(ctx, nil, "")
	if err != nil || !a.Authenticated {
		t.Fatalf("IsAuthenticated: %v %v", a, err)
	}
}
