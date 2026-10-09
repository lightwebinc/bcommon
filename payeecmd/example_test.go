package payeecmd_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lightwebinc/bcommon/payeecmd"
)

// An application binds the verbs to its own flag set and runs the one the
// arguments name. Here a misuse is refused before any home is opened.
func ExampleCommand() {
	c := &payeecmd.Command{App: "sample", KeyDir: os.TempDir(), Stdout: os.Stdout}
	fs := flag.NewFlagSet("payee", flag.ContinueOnError)
	f := c.Bind(fs)
	_ = fs.Parse([]string{"-in-flight", "0", "settle", "payments.jsonl"})
	fmt.Println(c.Run(context.Background(), f, fs.Args()))
	fmt.Println(strings.SplitN(c.Help(), "\n", 2)[0])
	// Output:
	// -in-flight 0 is outside 1 to 64: payments broadcast and not yet mined at once
	// usage: sample payee key -out FILE
}
