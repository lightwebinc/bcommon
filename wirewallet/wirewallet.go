// Package wirewallet reaches a BRC-100 wallet over the wallet wire, the
// binary substrate both SDKs implement, and answers the one question an
// application asks a wallet before anything else: whose identity is this.
//
// The wire is the only remote substrate here. The SDK's JSON substrate
// serialises the derivation arguments wrongly (a value receiver meets a
// pointer MarshalJSON) and drops action options, and both failures are
// silent; the wire is one serializer on both ends.
package wirewallet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/bsv-blockchain/go-sdk/wallet/serializer"
	"github.com/bsv-blockchain/go-sdk/wallet/substrates"
)

// ErrNotLoopback refuses a wallet that is not on this machine. The wire
// carries no authentication and no confidentiality, so a wallet reachable
// over a network is a key-use oracle for anyone who can reach it.
var ErrNotLoopback = errors.New("wirewallet: the wallet URL must be loopback; tunnel a remote wallet rather than exposing it")

// CheckURL accepts an http URL whose host is a loopback address or
// "localhost". Anything else is ErrNotLoopback.
func CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("wirewallet: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("wirewallet: scheme %q; want http", u.Scheme)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return ErrNotLoopback
	}
	return nil
}

// Dial returns a wallet.Interface that speaks to the wallet at base over the
// wire, presenting originator on every call. It makes no request itself.
func Dial(originator, base string, timeout time.Duration) (wallet.Interface, error) {
	if err := CheckURL(base); err != nil {
		return nil, err
	}
	wire := substrates.NewHTTPWalletWire(originator, base, &http.Client{Timeout: timeout})
	return &substrates.WalletWireTransceiver{Wire: wire}, nil
}

// IdentityKeyOf asks the wallet for its identity key: the root every record
// derivation hangs from, and the key a reader resolves a name to.
func IdentityKeyOf(ctx context.Context, w wallet.Interface, originator string) (*ec.PublicKey, error) {
	res, err := w.GetPublicKey(ctx, wallet.GetPublicKeyArgs{IdentityKey: true}, originator)
	if err != nil {
		return nil, fmt.Errorf("wirewallet: identity key: %w", err)
	}
	if res == nil || res.PublicKey == nil {
		return nil, errors.New("wirewallet: the wallet answered no identity key")
	}
	return res.PublicKey, nil
}

// callByName is the HTTP wire's routing table: the SDK's client POSTs each
// call's parameters to base/<name> with the originator in the Origin header,
// and the server rebuilds the request frame the processor expects.
var callByName = map[string]substrates.Call{
	"createAction":                 substrates.CallCreateAction,
	"signAction":                   substrates.CallSignAction,
	"abortAction":                  substrates.CallAbortAction,
	"listActions":                  substrates.CallListActions,
	"internalizeAction":            substrates.CallInternalizeAction,
	"listOutputs":                  substrates.CallListOutputs,
	"relinquishOutput":             substrates.CallRelinquishOutput,
	"getPublicKey":                 substrates.CallGetPublicKey,
	"revealCounterpartyKeyLinkage": substrates.CallRevealCounterpartyKeyLinkage,
	"revealSpecificKeyLinkage":     substrates.CallRevealSpecificKeyLinkage,
	"encrypt":                      substrates.CallEncrypt,
	"decrypt":                      substrates.CallDecrypt,
	"createHmac":                   substrates.CallCreateHMAC,
	"verifyHmac":                   substrates.CallVerifyHMAC,
	"createSignature":              substrates.CallCreateSignature,
	"verifySignature":              substrates.CallVerifySignature,
	"acquireCertificate":           substrates.CallAcquireCertificate,
	"listCertificates":             substrates.CallListCertificates,
	"proveCertificate":             substrates.CallProveCertificate,
	"relinquishCertificate":        substrates.CallRelinquishCertificate,
	"discoverByIdentityKey":        substrates.CallDiscoverByIdentityKey,
	"discoverByAttributes":         substrates.CallDiscoverByAttributes,
	"isAuthenticated":              substrates.CallIsAuthenticated,
	"waitForAuthentication":        substrates.CallWaitForAuthentication,
	"getHeight":                    substrates.CallGetHeight,
	"getHeaderForHeight":           substrates.CallGetHeaderForHeight,
	"getNetwork":                   substrates.CallGetNetwork,
	"getVersion":                   substrates.CallGetVersion,
}

// Serve is the other end of the wire: an HTTP handler that routes the SDK
// client's calls to w. It exists so that any wallet.Interface, an embedded
// one included, can be served as a wallet an application drives over the
// wire, which is how the transport is tested without a third party.
func Serve(w wallet.Interface) http.Handler {
	proc := substrates.NewWalletWireProcessor(w)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(rw, "wallet wire: POST a call", http.StatusMethodNotAllowed)
			return
		}
		call, ok := callByName[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.Error(rw, "wallet wire: unknown call", http.StatusNotFound)
			return
		}
		params, err := readAtMost(r.Body, 1<<20)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		frame := serializer.WriteRequestFrame(serializer.RequestFrame{
			Call: byte(call), Originator: r.Header.Get("Origin"), Params: params,
		})
		out, err := proc.TransmitToWallet(r.Context(), frame)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		rw.Header().Set("Content-Type", "application/octet-stream")
		_, _ = rw.Write(out)
	})
}
