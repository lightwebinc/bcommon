package bwallet

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/pushdrop"
)

// Profile is what makes a wallet one application's: the derivation of its
// funding key, the basket its coin is listed under, the version it answers,
// and the name its coin file had before wallet.json.
//
// It is required and has no default. Each field is fixed by what already
// exists outside the program: coin is paid to the fund key, other wallets
// and tools look that coin up by the basket, and a directory whose coin still
// sits under the legacy name opens empty if the name is wrong, which reads
// exactly like spent coin. A default would let a caller that forgot the profile
// derive some other application's key without an error.
type Profile struct {
	// FundProtocol and FundKeyID derive the funding key, always with the
	// counterparty self. Changing either after the first coin is paid to
	// the key strands that coin. The security level is taken as given:
	// BRC-43's level 0 is valid, and Validate cannot tell it from a level
	// left unset, so a profile states its level and 0 is derived under as
	// 0, never replaced by a default. Validate does refuse a level outside
	// 0..2, and a protocol name or key id the SDK's key deriver would
	// refuse, so such a profile fails when a wallet is opened rather than
	// at its first fund derivation.
	FundProtocol wallet.Protocol
	FundKeyID    string
	// FundBasket is the one basket ListOutputs answers for. BRC-100 basket
	// names allow lowercase letters, digits and spaces only.
	FundBasket string
	// Version is what GetVersion answers.
	Version string
	// LegacyPoolFile is the name the coin file had before it was
	// wallet.json. Open and OpenPool rename a file left under it, in place,
	// when the directory holds no wallet.json.
	//
	// It is for an application whose existing wallet directories keep coin
	// under an earlier name: without the rename such a directory opens as an
	// empty wallet, which reads exactly like spent coin. It is required,
	// rather than empty meaning none, so that such an application cannot leave
	// it out by accident. An application with no earlier name puts a file name
	// of its own there that no program writes into the wallet's directory,
	// such as "<application>-pool.json"; the file never exists, so nothing is
	// ever adopted. Any file found under the name is adopted as the coin file.
	LegacyPoolFile string
}

// ErrProfile refuses a Profile with a field missing or unusable. The wrapped
// error names the field; a fund derivation the SDK would refuse also wraps
// pushdrop.ErrDerivation.
var ErrProfile = errors.New("bwallet: wallet profile incomplete")

// Validate reports the first field of p that is missing or unusable.
//
// The legacy name must be a bare file name and neither of the wallet's own
// files: adoption renames it over wallet.json, so naming identity.json
// there would move the root key out from under the wallet.
func (p Profile) Validate() error {
	switch {
	case p.FundProtocol.Protocol == "":
		return fmt.Errorf("%w: FundProtocol has no protocol name", ErrProfile)
	case p.FundKeyID == "":
		return fmt.Errorf("%w: FundKeyID is empty", ErrProfile)
	}
	// The fund derivation meets BRC-43 by the rules the SDK's key deriver
	// applies. pushdrop's Validate states them without a counterparty, and
	// the fund key's counterparty self does not enter them.
	if err := (pushdrop.Derivation{Protocol: p.FundProtocol, KeyID: p.FundKeyID}).Validate(); err != nil {
		return fmt.Errorf("%w: FundProtocol and FundKeyID: %w", ErrProfile, err)
	}
	switch {
	case p.FundBasket == "":
		return fmt.Errorf("%w: FundBasket is empty", ErrProfile)
	case p.Version == "":
		return fmt.Errorf("%w: Version is empty", ErrProfile)
	case p.LegacyPoolFile == "":
		return fmt.Errorf("%w: LegacyPoolFile is empty", ErrProfile)
	case p.LegacyPoolFile != filepath.Base(p.LegacyPoolFile) || p.LegacyPoolFile == "." || p.LegacyPoolFile == "..":
		return fmt.Errorf("%w: LegacyPoolFile %q is not a file name", ErrProfile, p.LegacyPoolFile)
	case p.LegacyPoolFile == walletFile || p.LegacyPoolFile == identityFile:
		return fmt.Errorf("%w: LegacyPoolFile %q is one of the wallet's own files", ErrProfile, p.LegacyPoolFile)
	}
	return nil
}
