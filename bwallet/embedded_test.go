package bwallet

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"
)

// testProfile is a neutral application's profile. Every field differs from
// any real application's, the security level included, so a value hard-wired
// in the package instead of read from the profile fails these tests.
var testProfile = Profile{
	FundProtocol:   wallet.Protocol{SecurityLevel: wallet.SecurityLevelEveryAppAndCounterparty, Protocol: "example app"},
	FundKeyID:      "coin",
	FundBasket:     "example coin",
	Version:        "example-embedded-1",
	LegacyPoolFile: "coins.json",
}

func newWallet(t *testing.T) *Embedded {
	t.Helper()
	e, err := Create(filepath.Join(t.TempDir(), "w"), testProfile)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func TestCreateOpenRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "w")
	e, err := Create(dir, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if got := mode(t, dir); got != 0o700 {
		t.Fatalf("dir mode %o, want 0700", got)
	}
	for _, f := range []string{identityFile, walletFile} {
		if got := mode(t, filepath.Join(dir, f)); got != 0o600 {
			t.Fatalf("%s mode %o, want 0600", f, got)
		}
	}
	// No temp file survives an atomic write.
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}

	again, err := Open(dir, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !again.IdentityKey().IsEqual(e.IdentityKey()) {
		t.Fatal("identity key changed across Open")
	}
	if _, err := Create(dir, testProfile); !errors.Is(err, ErrIdentityExists) {
		t.Fatalf("Create over an existing identity: err %v, want ErrIdentityExists", err)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "nothing"), testProfile); err == nil {
		t.Fatal("Open of a directory with no identity must fail, never silently create one")
	}
}

func TestFundScriptDeterministicAndDistinctFromIdentity(t *testing.T) {
	e := newWallet(t)
	s1, err := e.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Open(e.Dir(), testProfile)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := again.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(*s1) != hex.EncodeToString(*s2) {
		t.Fatal("fund script differs across Open")
	}
	if len(*s1) != 25 {
		t.Fatalf("fund script is %d bytes, want a 25-byte P2PKH", len(*s1))
	}
	fk, err := e.FundKey()
	if err != nil {
		t.Fatal(err)
	}
	if fk.IsEqual(e.IdentityKey()) {
		t.Fatal("fund key must be a derived key, not the identity key")
	}
	a1, err := e.FundAddress(false)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := again.FundAddress(false)
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 || a1 == "" {
		t.Fatalf("fund address %q vs %q", a1, a2)
	}
	// The derivation is the SDK's under the profile's triple: an independent
	// KeyDeriver over the same root agrees with what GetPublicKey answered.
	raw, err := os.ReadFile(filepath.Join(e.Dir(), identityFile))
	if err != nil {
		t.Fatal(err)
	}
	var id identityJSON
	if err := json.Unmarshal(raw, &id); err != nil {
		t.Fatal(err)
	}
	root, err := ec.PrivateKeyFromWif(id.WIF)
	if err != nil {
		t.Fatal(err)
	}
	want, err := wallet.NewKeyDeriver(root).DerivePublicKey(testProfile.FundProtocol, testProfile.FundKeyID, fundCounterparty, true)
	if err != nil {
		t.Fatal(err)
	}
	if !want.IsEqual(fk) {
		t.Fatal("fund key is not derive(root, [2,\"example app\"], \"coin\", self)")
	}
}

// spendFundOutput builds a transaction spending a fake output locked to
// w's fund script, signed by signer's unlocker. Separating the two lets the
// control case sign the right output with the wrong wallet.
func spendFundOutput(t *testing.T, w, signer *Embedded) (*transaction.Transaction, *transaction.TransactionOutput) {
	t.Helper()
	lock, err := w.FundScript()
	if err != nil {
		t.Fatal(err)
	}
	parent := transaction.NewTransaction()
	parent.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: lock})

	tx := transaction.NewTransaction()
	if err := tx.AddInputFrom(parent.TxID().String(), 0, hex.EncodeToString(*lock), 1000, signer.FundUnlocker()); err != nil {
		t.Fatal(err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 900, LockingScript: lock})
	if err := tx.Sign(); err != nil {
		t.Fatal(err)
	}
	return tx, parent.Outputs[0]
}

func execute(tx *transaction.Transaction, prev *transaction.TransactionOutput) error {
	return interpreter.NewEngine().Execute(
		interpreter.WithTx(tx, 0, prev),
		interpreter.WithForkID(),
		interpreter.WithAfterGenesis(),
		interpreter.WithAfterChronicle(),
	)
}

func TestFundUnlockerIsAcceptedByTheInterpreter(t *testing.T) {
	e := newWallet(t)
	tx, prev := spendFundOutput(t, e, e)
	if tx.Inputs[0].UnlockingScript == nil || len(*tx.Inputs[0].UnlockingScript) == 0 {
		t.Fatal("Sign left the unlocking script empty")
	}
	if err := execute(tx, prev); err != nil {
		t.Fatalf("interpreter rejected the fund unlocker's script: %v", err)
	}
	if est := e.FundUnlocker().EstimateLength(tx, 0); est != 106 {
		t.Fatalf("EstimateLength %d, want 106", est)
	}

	// Control: the same output signed by a different wallet must fail, or
	// the acceptance above proves nothing about the key.
	other := newWallet(t)
	bad, prev := spendFundOutput(t, e, other)
	if err := execute(bad, prev); err == nil {
		t.Fatal("interpreter accepted a script signed by the wrong wallet")
	}
}

// The fund counterparty is fixed at self by the library, not by the profile,
// so the library holds it itself. TestFundScriptDeterministicAndDistinctFromIdentity
// derives through the package variable and would follow an edit to it; this
// one names self as a literal.
// The root is 0x42 repeated because at root 1 the identity key is the
// generator, BRC-42's "anyone" key, and self and anyone derive alike.
func TestFundCounterpartyIsSelf(t *testing.T) {
	root, err := ec.PrivateKeyFromHex("4242424242424242424242424242424242424242424242424242424242424242")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "w")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(identityJSON{WIF: root.Wif()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, identityFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := Open(dir, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	self, err := wallet.NewKeyDeriver(root).DerivePublicKey(testProfile.FundProtocol, testProfile.FundKeyID,
		wallet.Counterparty{Type: wallet.CounterpartyTypeSelf}, true)
	if err != nil {
		t.Fatal(err)
	}
	anyone, err := wallet.NewKeyDeriver(root).DerivePublicKey(testProfile.FundProtocol, testProfile.FundKeyID,
		wallet.Counterparty{Type: wallet.CounterpartyTypeAnyone}, true)
	if err != nil {
		t.Fatal(err)
	}
	if self.IsEqual(anyone) {
		t.Fatal("at this root self and anyone derive the same key, so the test cannot see the counterparty")
	}
	fk, err := e.FundKey()
	if err != nil {
		t.Fatal(err)
	}
	if !fk.IsEqual(self) {
		t.Fatal("fund key is not derive(root, profile protocol, profile key id, self)")
	}

	// The unlocker signs under self too: coin paid to the self-derived key
	// is spent by FundUnlocker.
	addr, err := script.NewAddressFromPublicKey(self, false)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		t.Fatal(err)
	}
	parent := transaction.NewTransaction()
	parent.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: lock})
	spend := transaction.NewTransaction()
	spend.AddInputFromTx(parent, 0, e.FundUnlocker())
	spend.AddOutput(&transaction.TransactionOutput{Satoshis: 900, LockingScript: lock})
	if err := spend.Sign(); err != nil {
		t.Fatal(err)
	}
	if err := execute(spend, parent.Outputs[0]); err != nil {
		t.Fatalf("the fund unlocker cannot spend coin paid to the self-derived key: %v", err)
	}
}

func TestFundUnlockerRefusesInputWithoutSource(t *testing.T) {
	e := newWallet(t)
	tx := transaction.NewTransaction()
	tx.AddInput(&transaction.TransactionInput{})
	if _, err := e.FundUnlocker().Sign(tx, 0); err == nil {
		t.Fatal("signing an input with no source output must fail, not sign over zero")
	}
	if _, err := e.FundUnlocker().Sign(tx, 5); err == nil {
		t.Fatal("out-of-range input must be an error")
	}
	// 1<<31 is negative as a 32-bit int, so a check that converted the index
	// to int would pass it there and the index would panic.
	if _, err := e.FundUnlocker().Sign(tx, 1<<31); err == nil || err.Error() != "bwallet: input 2147483648 out of range" {
		t.Fatalf("input 1<<31: %v", err)
	}
}

// The wallet abstraction's promise is that no private key leaves it. The
// SDK's ProtoWallet keeps the root inside an unexported deriver; this checks
// that Embedded itself adds no field, exported or not, of a private-key type.
func TestEmbeddedHoldsNoPrivateKeyField(t *testing.T) {
	pk := reflect.TypeFor[*ec.PrivateKey]()
	typ := reflect.TypeFor[Embedded]()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type == pk || f.Type == pk.Elem() {
			t.Fatalf("Embedded.%s is a private key; keys stay inside the deriver", f.Name)
		}
	}
}

// A wallet written under the previous file name must survive being opened,
// because a wallet file that is not found reads as an empty wallet, and an
// empty wallet is indistinguishable from coin that has been spent.
func TestOpenAdoptsAWalletLeftUnderTheOldName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "w")
	e, err := Create(dir, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Add(Output{TxID: "aa", Vout: 0, Satoshis: 5000, Height: 7}); err != nil {
		t.Fatal(err)
	}
	// Put the file back under the name a previous version wrote.
	legacyWalletFile := testProfile.LegacyPoolFile
	if err := os.Rename(filepath.Join(dir, walletFile), filepath.Join(dir, legacyWalletFile)); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Pool.Balance(); got != 5000 {
		t.Fatalf("balance %d after adopting the old file, want 5000", got)
	}
	if _, err := os.Stat(filepath.Join(dir, walletFile)); err != nil {
		t.Fatalf("the old file was not renamed to %s: %v", walletFile, err)
	}
	// Exactly one file, so two names can never hold different outputs.
	if _, err := os.Stat(filepath.Join(dir, legacyWalletFile)); !os.IsNotExist(err) {
		t.Fatalf("%s still exists after the rename", legacyWalletFile)
	}
}

// A wallet under the current name is used as it stands, and a stray file under
// the old name is left alone rather than overwriting it.
func TestOpenPrefersTheCurrentWalletFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "w")
	e, err := Create(dir, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Pool.Add(Output{TxID: "bb", Vout: 0, Satoshis: 1234, Height: 3}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testProfile.LegacyPoolFile), []byte(`{"outputs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Pool.Balance(); got != 1234 {
		t.Fatalf("balance %d, want the current file's 1234", got)
	}
}
