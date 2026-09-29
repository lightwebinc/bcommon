package bwallet

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/pushdrop"
)

// incompleteProfiles is the zero Profile and testProfile with each field
// missing in turn, naming a legacy file the wallet cannot adopt safely, or
// complete but with a fund derivation BRC-43 refuses. The last kind is named
// "fund derivation ..." and is refused by pushdrop's rules and by the SDK's
// key deriver as well. Each row carries the text its refusal must contain,
// which names the field at fault.
func incompleteProfiles() map[string]struct {
	p     Profile
	names string
} {
	cut := func(f func(*Profile)) Profile {
		p := testProfile
		f(&p)
		return p
	}
	const fund = "FundProtocol and FundKeyID: "
	return map[string]struct {
		p     Profile
		names string
	}{
		"zero":                     {Profile{}, "FundProtocol has no protocol name"},
		"no protocol name":         {cut(func(p *Profile) { p.FundProtocol.Protocol = "" }), "FundProtocol has no protocol name"},
		"no key id":                {cut(func(p *Profile) { p.FundKeyID = "" }), "FundKeyID is empty"},
		"no basket":                {cut(func(p *Profile) { p.FundBasket = "" }), "FundBasket is empty"},
		"no version":               {cut(func(p *Profile) { p.Version = "" }), "Version is empty"},
		"no legacy file":           {cut(func(p *Profile) { p.LegacyPoolFile = "" }), "LegacyPoolFile is empty"},
		"legacy file is a path":    {cut(func(p *Profile) { p.LegacyPoolFile = "old/coins.json" }), `LegacyPoolFile "old/coins.json" is not a file name`},
		"legacy file is dot":       {cut(func(p *Profile) { p.LegacyPoolFile = "." }), `LegacyPoolFile "." is not a file name`},
		"legacy file is dotdot":    {cut(func(p *Profile) { p.LegacyPoolFile = ".." }), `LegacyPoolFile ".." is not a file name`},
		"legacy file is the key":   {cut(func(p *Profile) { p.LegacyPoolFile = identityFile }), `LegacyPoolFile "identity.json" is one of the wallet's own files`},
		"legacy file is the coins": {cut(func(p *Profile) { p.LegacyPoolFile = walletFile }), `LegacyPoolFile "wallet.json" is one of the wallet's own files`},

		"fund derivation level 3":             {cut(func(p *Profile) { p.FundProtocol.SecurityLevel = 3 }), fund},
		"fund derivation level -1":            {cut(func(p *Profile) { p.FundProtocol.SecurityLevel = -1 }), fund},
		"fund derivation short name":          {cut(func(p *Profile) { p.FundProtocol.Protocol = "app" }), fund},
		"fund derivation name punctuation":    {cut(func(p *Profile) { p.FundProtocol.Protocol = "example-app" }), fund},
		"fund derivation name double space":   {cut(func(p *Profile) { p.FundProtocol.Protocol = "example  app" }), fund},
		"fund derivation name ends protocol":  {cut(func(p *Profile) { p.FundProtocol.Protocol = "example protocol" }), fund},
		"fund derivation key id over 800":     {cut(func(p *Profile) { p.FundKeyID = strings.Repeat("k", 801) }), fund},
		"fund derivation blank name, key set": {cut(func(p *Profile) { p.FundProtocol.Protocol = "   " }), fund},
	}
}

// Every constructor refuses a profile it cannot use, before it touches the
// disk. A default here would derive some other application's fund key for
// a caller that forgot to pass one, and pay coin to it.
func TestConstructorsRefuseAnIncompleteProfile(t *testing.T) {
	if err := testProfile.Validate(); err != nil {
		t.Fatalf("the neutral profile is refused: %v", err)
	}
	primary := newWallet(t)
	idPath := filepath.Join(primary.Dir(), identityFile)
	for name, row := range incompleteProfiles() {
		t.Run(name, func(t *testing.T) {
			p := row.p
			// refused is ErrProfile naming the field at fault: a caller
			// told only that the profile is incomplete cannot tell which
			// of five fields to fix.
			refused := func(err error) bool {
				return errors.Is(err, ErrProfile) && strings.Contains(err.Error(), ": "+row.names)
			}
			err := p.Validate()
			if !refused(err) {
				t.Fatalf("Validate: %v, want ErrProfile naming %q", err, row.names)
			}
			if strings.HasPrefix(name, "fund derivation ") {
				if !errors.Is(err, pushdrop.ErrDerivation) {
					t.Fatalf("Validate: %v, want pushdrop.ErrDerivation as well", err)
				}
				// The SDK's key deriver refuses the same derivation, so
				// the row is one no wallet could derive, not a profile
				// Validate refuses on rules of its own.
				forSelf := true
				if _, err := primary.GetPublicKey(context.Background(), wallet.GetPublicKeyArgs{
					EncryptionArgs: wallet.EncryptionArgs{ProtocolID: p.FundProtocol, KeyID: p.FundKeyID, Counterparty: fundCounterparty},
					ForSelf:        &forSelf,
				}, ""); err == nil {
					t.Fatal("the SDK derives this fund key, so Validate refuses a profile the wallet could use")
				}
			}
			fresh := filepath.Join(t.TempDir(), "w")
			if _, err := Create(fresh, p); !refused(err) {
				t.Errorf("Create: %v, want ErrProfile naming %q", err, row.names)
			}
			if _, err := OpenPool(fresh, p); !refused(err) {
				t.Errorf("OpenPool: %v, want ErrProfile naming %q", err, row.names)
			}
			if _, err := os.Stat(fresh); !os.IsNotExist(err) {
				t.Errorf("a refused profile left %s behind (stat err %v)", fresh, err)
			}
			if _, err := Open(primary.Dir(), p); !refused(err) {
				t.Errorf("Open: %v, want ErrProfile naming %q", err, row.names)
			}
			if _, err := OpenIdentity(idPath, primary.Pool, p); !refused(err) {
				t.Errorf("OpenIdentity: %v, want ErrProfile naming %q", err, row.names)
			}
			// The directory the refusals were pointed at is as it was: in
			// particular a legacy name of identity.json did not move the key.
			if _, err := os.Stat(idPath); err != nil {
				t.Fatalf("identity file gone after a refused open: %v", err)
			}
		})
	}
}

// A level 0 profile is accepted by every constructor and derives under
// level 0: the wallet puts no level of its own in place of a zero one.
func TestSecurityLevelZeroIsTakenAsGiven(t *testing.T) {
	silent := testProfile
	silent.FundProtocol.SecurityLevel = wallet.SecurityLevelSilent
	if err := silent.Validate(); err != nil {
		t.Fatalf("Validate refused level 0: %v", err)
	}
	e, err := Create(filepath.Join(t.TempDir(), "w"), silent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(e.Dir(), silent); err != nil {
		t.Fatalf("Open refused level 0: %v", err)
	}
	if _, err := OpenIdentity(filepath.Join(e.Dir(), identityFile), e.Pool, silent); err != nil {
		t.Fatalf("OpenIdentity refused level 0: %v", err)
	}
	if _, err := OpenPool(e.Dir(), silent); err != nil {
		t.Fatalf("OpenPool refused level 0: %v", err)
	}
	got, err := e.FundKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, lvl := range []wallet.SecurityLevel{wallet.SecurityLevelEveryApp, wallet.SecurityLevelEveryAppAndCounterparty} {
		other := silent
		other.FundProtocol.SecurityLevel = lvl
		k, err := (&Signer{Interface: e, Identity: e.IdentityKey(), Profile: other}).FundKey()
		if err != nil {
			t.Fatal(err)
		}
		if k.IsEqual(got) {
			t.Errorf("the level 0 fund key is the level %d key", lvl)
		}
	}
}

// A Signer carries its profile, and the fund methods refuse one without it,
// while the calls that never touch the fund key still answer. A wallet
// assembled without a constructor refuses the two methods that answer from
// its profile.
func TestSignerWithoutAProfileRefusesTheFundKey(t *testing.T) {
	ctx := context.Background()
	e := newWallet(t)
	s := &Signer{Interface: e, Identity: e.IdentityKey()}
	if _, err := s.FundKey(); !errors.Is(err, ErrProfile) {
		t.Errorf("FundKey: %v, want ErrProfile", err)
	}
	if _, err := s.FundScript(); !errors.Is(err, ErrProfile) {
		t.Errorf("FundScript: %v, want ErrProfile", err)
	}
	if _, err := s.FundAddress(false); !errors.Is(err, ErrProfile) {
		t.Errorf("FundAddress: %v, want ErrProfile", err)
	}
	lock, err := e.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	tx := transaction.NewTransaction()
	if err := tx.AddInputFrom(hex.EncodeToString(make([]byte, 32)), 0, hex.EncodeToString(*lock), 1000, s.FundUnlocker()); err != nil {
		t.Fatal(err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 900, LockingScript: lock})
	if err := tx.Sign(); !errors.Is(err, ErrProfile) {
		t.Errorf("signing with the fund unlocker: %v, want ErrProfile", err)
	}

	other := newWallet(t)
	if _, err := s.PaymentDestination(ctx, other.Signer().IdentityHex(), "p", "s"); err != nil {
		t.Errorf("PaymentDestination needs no profile: %v", err)
	}

	bare := &Embedded{ProtoWallet: e.ProtoWallet, Pool: e.Pool, identity: e.IdentityKey()}
	if _, err := bare.ListOutputs(ctx, wallet.ListOutputsArgs{}, ""); !errors.Is(err, ErrProfile) {
		t.Errorf("ListOutputs without a profile: %v, want ErrProfile", err)
	}
	if _, err := bare.GetVersion(ctx, nil, ""); !errors.Is(err, ErrProfile) {
		t.Errorf("GetVersion without a profile: %v, want ErrProfile", err)
	}
}

// Each field of the profile is read, never a value of the package's own: a
// change to any one field changes what the wallet does with it, through every
// constructor that takes it.
func TestEveryProfileFieldIsRead(t *testing.T) {
	ctx := context.Background()
	change := func(f func(*Profile)) Profile {
		p := testProfile
		f(&p)
		return p
	}
	base := newWallet(t)
	idPath := filepath.Join(base.Dir(), identityFile)
	fundKey := func(t *testing.T, p Profile) string {
		t.Helper()
		k, err := (&Signer{Interface: base, Identity: base.IdentityKey(), Profile: p}).FundKey()
		if err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(k.Compressed())
	}
	baseKey := fundKey(t, testProfile)

	// The fund key follows the protocol's level and name and the key id,
	// under every way a wallet is opened.
	for name, p := range map[string]Profile{
		"security level": change(func(p *Profile) { p.FundProtocol.SecurityLevel = wallet.SecurityLevelEveryApp }),
		"protocol name":  change(func(p *Profile) { p.FundProtocol.Protocol = "another app" }),
		"key id":         change(func(p *Profile) { p.FundKeyID = "other" }),
	} {
		t.Run("fund key/"+name, func(t *testing.T) {
			want := fundKey(t, p)
			if want == baseKey {
				t.Fatalf("changing the %s left the fund key unchanged", name)
			}
			opened, err := Open(base.Dir(), p)
			if err != nil {
				t.Fatal(err)
			}
			rotated, err := OpenIdentity(idPath, base.Pool, p)
			if err != nil {
				t.Fatal(err)
			}
			for how, e := range map[string]*Embedded{"Open": opened, "OpenIdentity": rotated} {
				k, err := e.FundKey()
				if err != nil {
					t.Fatal(err)
				}
				if got := hex.EncodeToString(k.Compressed()); got != want {
					t.Errorf("%s derived %s under the changed %s, want %s", how, got, name, want)
				}
			}
		})
	}

	// The basket listed and the version answered follow the profile.
	p := change(func(p *Profile) { p.FundBasket, p.Version = "another coin", "another-embedded-2" })
	e, err := Open(base.Dir(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Add(out(0xcc, 1, 5000, 7, false)); err != nil {
		t.Fatal(err)
	}
	for basket, want := range map[string]int{"another coin": 1, testProfile.FundBasket: 0} {
		res, err := e.ListOutputs(ctx, wallet.ListOutputsArgs{Basket: basket}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Outputs) != want {
			t.Errorf("ListOutputs(basket %q) listed %d, want %d", basket, len(res.Outputs), want)
		}
	}
	if v, err := e.GetVersion(ctx, nil, ""); err != nil || v.Version != "another-embedded-2" {
		t.Errorf("GetVersion: %v %v, want the profile's version", v, err)
	}

	// The legacy file adopted is the profile's, and only the profile's: a
	// file under another application's old name is left where it lies.
	const oldCoins = `{"outputs":[{"txid":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","vout":0,"satoshis":700,"lockingScript":"51"}]}`
	for _, tc := range []struct {
		file string
		want uint64
	}{{"older.json", 700}, {testProfile.LegacyPoolFile, 0}} {
		dir := filepath.Join(t.TempDir(), "w")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(oldCoins), 0o600); err != nil {
			t.Fatal(err)
		}
		pool, err := OpenPool(dir, change(func(p *Profile) { p.LegacyPoolFile = "older.json" }))
		if err != nil {
			t.Fatal(err)
		}
		if got := pool.Balance(); got != tc.want {
			t.Errorf("a directory with coin under %s opened with balance %d, want %d", tc.file, got, tc.want)
		}
	}
}
