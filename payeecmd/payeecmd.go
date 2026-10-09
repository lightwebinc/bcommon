// Package payeecmd is the payee verbs of an application's command line, the
// same in every application: payee key, which hands a host its payee key,
// and payee settle, which takes the payments a host recorded into the
// payee's pool. Package payee does the work; this package is the verbs, their
// flags, their help and their words.
//
// An application gives a Command its name, the directory that holds the
// payee's identity file, its output streams, the errors that carry its exit
// codes, and a way to open its home for settle. It keeps its own flag set and
// parsing, which this package binds to through FlagSet:
//
//	c := &payeecmd.Command{App: "app", KeyDir: home, Stdout: stdout, Stderr: stderr,
//		Usage: usage, Refused: refused, Open: openForSettle}
//	fs := flag.NewFlagSet("payee", flag.ContinueOnError)
//	fs.Usage = func() { fmt.Fprintln(stderr, c.Help()) }
//	f := c.Bind(fs)
//	if err := fs.Parse(args); err != nil {
//		return err
//	}
//	return c.Run(ctx, f, fs.Args())
package payeecmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lightwebinc/bcommon/payee"
)

// Session is what settle needs of an opened home: the payee's purse, its
// record of what is settled, its pool (reported when set), and Close, which
// settle calls when it is done (nil is nothing to close).
type Session struct {
	Payer  payee.Payer
	Record payee.Record
	Pool   payee.Pool
	Close  func()
}

// Command is one application's payee verbs.
type Command struct {
	// App is the command's name, as a user types it: it names the host's
	// key variable (payee.KeyEnv), the command that creates a missing home
	// (`<App> init`), the help, and the labels a settled payment carries.
	App string
	// KeyDir is the directory that holds the home's identity file
	// (payee.IdentityFile), whose key is the payee key.
	KeyDir string
	// Stdout takes the verbs' output. Stderr takes notes and the lines of
	// payments not settled; nil discards them.
	Stdout, Stderr io.Writer
	// Note, when set, writes a note in the application's way (a quiet flag,
	// a prefix); nil writes the note and a newline to Stderr.
	Note func(format string, args ...any)
	// Usage and Refused make the application's errors for a misuse and for
	// a refusal, which carry its exit codes; nil is a plain error.
	Usage, Refused func(format string, args ...any) error
	// Words, when set, puts a refusal of the purse in the application's
	// words (payee.Settler.Words).
	Words func(error) error
	// Open opens the home for settle.
	Open func(ctx context.Context) (*Session, error)
}

// FlagSet is the part of a flag set Bind uses; *flag.FlagSet is one.
type FlagSet interface {
	StringVar(p *string, name, value, usage string)
	IntVar(p *int, name string, value int, usage string)
}

// Flags are the verbs' flags, bound to an application's flag set.
type Flags struct {
	Out      string
	InFlight int
}

// Bind adds the verbs' flags to fs: -out (key) and -in-flight (settle).
func (c *Command) Bind(fs FlagSet) *Flags {
	f := &Flags{}
	fs.StringVar(&f.Out, "out", "", "key: the file to write "+payee.KeyEnv(c.App)+" to")
	fs.IntVar(&f.InFlight, "in-flight", payee.DefaultInFlight, "settle: the most payments broadcast and not yet mined at once")
	return f
}

// Help is the verbs' help, in the application's name.
func (c *Command) Help() string {
	return fmt.Sprintf(help, c.App, payee.KeyEnv(c.App), payee.DefaultInFlight, payee.MaxInFlight)
}

const help = `usage: %[1]s payee key -out FILE
       %[1]s payee settle <payments.jsonl>... [-in-flight N]

A host that prices a question is paid to its payee key (%[2]s):
each payment is a BRC-29 output to a key derived from it, recorded by the
host in payments.jsonl in its state directory before the question is
answered, and not broadcast by the host. Until it is settled, the payer can
still spend the coins elsewhere: a payment is money only once settled.

key writes %[2]s=<this home's identity private key> to FILE (mode
0600, never over an existing file), or with -out - to standard output, for
the host's environment: the home is then the payee's wallet.

settle takes every payment in the ledgers (one or more hosts' files; a
payment in two is settled once) that this home has not settled into the
home's wallet: each is checked to pay the key this identity derives for
its remittance and payer, verified against the headers, broadcast through
the settlement leg, waited for until it mines, and added to the pool, and
its txid is recorded in the home as settled. Every payment is broadcast
before any is waited for, so a run takes about one block however many
there are; -in-flight (default %[3]d, at most %[4]d) bounds how many are
broadcast and not yet mined at once. A payment the network refuses for
good (its payer spent the inputs elsewhere) is reported once, recorded,
and passed over by later runs. Run it on a schedule: the sooner a payment
is settled, the shorter the window in which the payer can take it back.`

// Run runs the verb pos names (pos is what is left after the flags): key,
// or settle with the ledgers' paths.
func (c *Command) Run(ctx context.Context, f *Flags, pos []string) error {
	switch {
	case len(pos) == 1 && pos[0] == "key":
		return c.key(f.Out)
	case len(pos) >= 2 && pos[0] == "settle":
		if f.InFlight < 1 || f.InFlight > payee.MaxInFlight {
			return c.usage("-in-flight %d is outside 1 to %d: payments broadcast and not yet mined at once", f.InFlight, payee.MaxInFlight)
		}
		return c.settle(ctx, pos[1:], f.InFlight)
	}
	return c.usage("payee key -out FILE | payee settle <payments.jsonl>...")
}

// key writes the home's identity private key as the host's key line.
func (c *Command) key(out string) error {
	if out == "" {
		return c.usage("payee key writes a secret: name a file with -out (it is created at mode 0600), or -out - to print it")
	}
	k, err := payee.HomeKey(c.KeyDir, c.App)
	if err != nil {
		return err
	}
	env := payee.KeyEnv(c.App)
	if out == "-" {
		fmt.Fprint(c.stdout(), payee.KeyLine(env, k))
		c.note("%s", payee.PrintedKeyNote)
		return nil
	}
	note, err := payee.CreateKeyFile(out, env, k)
	if errors.Is(err, payee.ErrKeyFileExists) {
		return c.usage("%s exists; payee key never writes over a file", out)
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(c.stdout(), note)
	return nil
}

// settle takes every payment in the ledgers this home has not.
func (c *Command) settle(ctx context.Context, paths []string, inFlight int) error {
	ps, err := payee.ReadLedgers(c.Stderr, paths...)
	if err != nil {
		return err
	}
	if c.Open == nil {
		return errors.New("payeecmd: no way to open the home for settle")
	}
	s, err := c.Open(ctx)
	if err != nil {
		return err
	}
	if s.Close != nil {
		defer s.Close()
	}
	rep, err := (&payee.Settler{App: c.App, Payer: s.Payer, Record: s.Record, Pool: s.Pool,
		InFlight: inFlight, Words: c.Words, Out: c.Stdout, Warn: c.Stderr}).Settle(ctx, ps)
	if err != nil {
		return err
	}
	if problem := rep.Problem(); problem != "" {
		if c.Refused != nil {
			return c.Refused("%s", problem)
		}
		return errors.New(problem)
	}
	return nil
}

func (c *Command) usage(format string, args ...any) error {
	if c.Usage != nil {
		return c.Usage(format, args...)
	}
	return fmt.Errorf(format, args...)
}

func (c *Command) note(format string, args ...any) {
	if c.Note != nil {
		c.Note(format, args...)
		return
	}
	if c.Stderr != nil {
		fmt.Fprintf(c.Stderr, format+"\n", args...)
	}
}

func (c *Command) stdout() io.Writer {
	if c.Stdout == nil {
		return io.Discard
	}
	return c.Stdout
}
