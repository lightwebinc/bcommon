package termsafe_test

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lightwebinc/bcommon/termsafe"
)

// A value from a record somebody else published is filtered before it is
// printed. The window-title sequence and the screen clear are dropped, the
// bidirectional override that would reverse the rest of the line is
// dropped, and the colour survives only when the reader asked for it.
func ExampleSanitize() {
	hostile := "status \x1b]0;you have been pwned\x07\x1b[2J\x1b[32mgreen\x1b[0m ‮evil"

	fmt.Printf("%q\n", termsafe.Text(hostile))
	fmt.Printf("%q\n", termsafe.Sanitize(hostile, termsafe.Options{ANSI: true}))
	fmt.Printf("%q\n", termsafe.Sanitize("café ☕", termsafe.Options{ASCII: true}))
	// Output:
	// "status green evil"
	// "status \x1b[32mgreen\x1b[0m evil\x1b[0m"
	// "caf? ?"
}

// The publishing side of the same rules: a value that a reader would have
// to strip is refused before it is published, naming the first offence. An
// application puts its own words in front and keeps both matches.
func ExampleValidateBounded() {
	errBody := errors.New("body text")
	for _, v := range []string{"back monday\n\x1b[1mbold\x1b[0m is fine", "ring\x07 the bell", "\x1b]0;title\x07"} {
		if err := termsafe.ValidateBounded("plan", v); err != nil {
			err = fmt.Errorf("%w: %w", errBody, err)
			fmt.Println(err, errors.Is(err, errBody), errors.Is(err, termsafe.ErrUnsafe))
			continue
		}
		fmt.Println("ok")
	}
	// Output:
	// ok
	// body text: plan line 1 has a control character (U+0007) true true
	// body text: plan line 1 has an escape sequence that is not a colour (SGR) true true
}

// Whether to degrade to ASCII is the locale's answer, read through the
// lookup the application passes: os.Getenv in a command, a map here.
func ExampleUTF8Locale() {
	env := map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}
	getenv := func(k string) string { return env[k] }

	fmt.Println(termsafe.UTF8Locale(getenv))
	fmt.Println(termsafe.Abbrev("02" + strings.Repeat("ab", 32)))
	// Output:
	// false
	// 02ababababab
}
