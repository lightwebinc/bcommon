package pushdrop

import (
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
)

// Template adapts the SDK's pushdrop.Unlocker to
// transaction.UnlockingScriptTemplate.
type Template struct {
	U *sdkpushdrop.Unlocker
}

var _ transaction.UnlockingScriptTemplate = Template{}

// Sign signs input inputIndex of tx through the wallet.
func (t Template) Sign(tx *transaction.Transaction, inputIndex uint32) (*script.Script, error) {
	return t.U.Sign(tx, int(inputIndex))
}

// EstimateLength is the unlocker's own estimate: one DER signature push.
func (t Template) EstimateLength(_ *transaction.Transaction, _ uint32) uint32 {
	return t.U.EstimateLength()
}
