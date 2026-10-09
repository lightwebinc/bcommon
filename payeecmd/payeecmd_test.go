package payeecmd_test

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/payee"
	"github.com/lightwebinc/bcommon/payeecmd"
	"github.com/lightwebinc/bcommon/purse"
)

const ledger = "../testdata/fixtures/payee/ledger-v2.jsonl"

// usageError and refusedError stand in for an application's errors.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

type refusedError struct{ msg string }

func (e refusedError) Error() string { return e.msg }

func command(t *testing.T, home string) (*payeecmd.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	return &payeecmd.Command{
		App: "sample", KeyDir: home, Stdout: &out, Stderr: &errOut,
		Usage:   func(f string, a ...any) error { return usageError{fmt.Sprintf(f, a...)} },
		Refused: func(f string, a ...any) error { return refusedError{fmt.Sprintf(f, a...)} },
	}, &out, &errOut
}

func run(t *testing.T, c *payeecmd.Command, args ...string) error {
	t.Helper()
	fs := flag.NewFlagSet("payee", flag.ContinueOnError)
	f := c.Bind(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return c.Run(context.Background(), f, fs.Args())
}

func homeWithKey(t *testing.T) (string, *ec.PrivateKey) {
	t.Helper()
	k, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, payee.IdentityFile), []byte(`{"wif":"`+k.Wif()+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, k
}

// The help names the application, its key variable and the in-flight bounds.
func TestHelp(t *testing.T) {
	c, _, _ := command(t, "")
	h := c.Help()
	for _, want := range []string{"usage: sample payee key -out FILE", "sample payee settle <payments.jsonl>...", "(SAMPLE_PAYEE_KEY)",
		fmt.Sprintf("(default %d, at most %d)", payee.DefaultInFlight, payee.MaxInFlight)} {
		if !strings.Contains(h, want) {
			t.Errorf("help does not say %q", want)
		}
	}
}

// key writes the line once at mode 0600, never over a file, prints it with
// -out - and its note, and refuses no -out; a home with no identity names
// the command that makes one.
func TestKey(t *testing.T) {
	home, k := homeWithKey(t)
	c, out, errOut := command(t, home)
	path := filepath.Join(t.TempDir(), "payee.env")
	if err := run(t, c, "-out", path, "key"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != payee.KeyLine("SAMPLE_PAYEE_KEY", k) {
		t.Fatalf("key file: %q %v", raw, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if !strings.Contains(out.String(), "wrote SAMPLE_PAYEE_KEY to "+path) {
		t.Fatalf("stdout: %q", out)
	}
	var ue usageError
	if err := run(t, c, "-out", path, "key"); !errors.As(err, &ue) || !strings.Contains(ue.msg, "never writes over a file") {
		t.Fatalf("over a file: %v", err)
	}
	out.Reset()
	if err := run(t, c, "-out", "-", "key"); err != nil || out.String() != payee.KeyLine("SAMPLE_PAYEE_KEY", k) ||
		strings.TrimSpace(errOut.String()) != payee.PrintedKeyNote {
		t.Fatalf("-out -: %v %q %q", err, out, errOut)
	}
	if err := run(t, c, "key"); !errors.As(err, &ue) || !strings.Contains(ue.msg, "name a file with -out") {
		t.Fatalf("no -out: %v", err)
	}
	c2, _, _ := command(t, t.TempDir())
	if err := run(t, c2, "-out", "-", "key"); err == nil || !strings.Contains(err.Error(), "run `sample init`") {
		t.Fatalf("no home: %v", err)
	}
}

// Anything but key or settle with a ledger, and an -in-flight out of bounds,
// is a misuse, refused before the home is opened.
func TestRunUsage(t *testing.T) {
	c, _, _ := command(t, "")
	c.Open = func(context.Context) (*payeecmd.Session, error) {
		t.Fatal("the home was opened")
		return nil, nil
	}
	for _, args := range [][]string{{}, {"settle"}, {"keys"}, {"key", "extra"}, {"-in-flight", "0", "settle", ledger},
		{"-in-flight", fmt.Sprint(payee.MaxInFlight + 1), "settle", ledger}} {
		var ue usageError
		if err := run(t, c, args...); !errors.As(err, &ue) {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// failing is a purse that checks no payment.
type failing struct{}

func (failing) Check(context.Context, wallet.InternalizeActionArgs) (*purse.Incoming, error) {
	return nil, errors.New("not this identity's")
}
func (failing) Broadcast(context.Context, *purse.Incoming) error { return nil }
func (failing) Await(context.Context, *purse.Incoming) error     { return nil }
func (failing) Take(*purse.Incoming) error                       { return nil }

// settle runs the ledgers through the opened home, closes it, and reports a
// payment not settled as the application's refusal; with every payment
// settled before, it succeeds and says so.
func TestSettle(t *testing.T) {
	ps, err := payee.ReadLedgers(nil, ledger)
	if err != nil || len(ps) == 0 {
		t.Fatal(len(ps), err)
	}
	c, out, errOut := command(t, "")
	var book payee.Book
	closed := 0
	c.Open = func(context.Context) (*payeecmd.Session, error) {
		return &payeecmd.Session{Payer: failing{}, Record: payee.Saved(&book, func() error { return nil }), Close: func() { closed++ }}, nil
	}
	var re refusedError
	if err := run(t, c, "-in-flight", "2", "settle", ledger); !errors.As(err, &re) || !strings.Contains(re.msg, "not settled") || closed != 1 {
		t.Fatalf("settle: %v, closed %d", err, closed)
	}
	if !strings.Contains(errOut.String(), "not this identity's") {
		t.Fatalf("stderr: %q", errOut)
	}
	for _, p := range ps {
		book.AddSettled(p.Txid)
	}
	out.Reset()
	if err := run(t, c, "settle", ledger, ledger); err != nil || closed != 2 ||
		!strings.Contains(out.String(), fmt.Sprintf("0 payment(s) settled, 0 sat; %d settled before", len(ps))) {
		t.Fatalf("settled before: %v %q", err, out)
	}
	c.Open = func(context.Context) (*payeecmd.Session, error) { return nil, errors.New("home in use") }
	if err := run(t, c, "settle", ledger); err == nil || err.Error() != "home in use" {
		t.Fatalf("open: %v", err)
	}
	if err := run(t, c, "settle", filepath.Join(t.TempDir(), "none.jsonl")); err == nil {
		t.Fatal("a missing ledger settled")
	}
}
