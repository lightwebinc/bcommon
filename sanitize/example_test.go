package sanitize_test

import (
	"fmt"

	"github.com/lightwebinc/bcommon/sanitize"
)

// A value from a record somebody else signed, filtered before a renderer
// shows it: the escape sequence, the bidirectional override and the hidden
// tag characters go, the tab becomes a space, and the emoji keep their
// selectors and joiner, and the flag its tags.
func ExampleFilter() {
	hostile := "ship it\t\x1b]0;pwned\x07\u202e!\U000E0069\U000E0067\U000E006E \u2764\ufe0f\u200d\U0001F525 " +
		"\U0001F3F4\U000E0067\U000E0062\U000E0077\U000E006C\U000E0073\U000E007F"
	fmt.Printf("%+q\n", sanitize.Filter(hostile))
	fmt.Printf("%+q\n", sanitize.Filter("a\u200db\ufe0f\ufe0f"))
	// Output:
	// "ship it ! \u2764\ufe0f\u200d\U0001f525 \U0001f3f4\U000e0067\U000e0062\U000e0077\U000e006c\U000e0073\U000e007f"
	// "ab"
}
