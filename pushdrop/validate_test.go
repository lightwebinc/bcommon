package pushdrop_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/pushdrop"
)

func derivation(level wallet.SecurityLevel, name, keyID string) pushdrop.Derivation {
	return pushdrop.Derivation{Protocol: wallet.Protocol{SecurityLevel: level, Protocol: name}, KeyID: keyID}
}

// sdkDerives asks the SDK itself, which stays the authority on the rules.
func sdkDerives(d pushdrop.Derivation) error {
	_, err := wallet.NewKeyDeriver(nil).DerivePublicKey(d.Protocol, d.KeyID, pushdrop.Anyone(), false)
	return err
}

// The rules the SDK applies, in its order, by the words Validate and the SDK
// each name them with.
var rules = []struct{ name, validate, sdk string }{
	{"security level", "security level", "security level must be"},
	{"key id too long", "over 800", "800 characters or less"},
	{"empty key id", "empty key id", "1 character or more"},
	{"name too long", "over 400", "400 characters or less"},
	{"linkage name too long", "over 430", "430 characters or less"},
	{"name too short", "under 5 characters", "5 characters or more"},
	{"consecutive spaces", "consecutive spaces", "consecutive spaces"},
	{"characters", "only letters, digits and spaces", "only contain letters, numbers and spaces"},
	{"protocol suffix", `ends in " protocol"`, `with " protocol"`},
}

// ruleOf is the rule a refusal's text names, from Validate's words or, with
// sdk, the SDK's. A text naming none of them, or more than one, says so.
func ruleOf(text string, sdk bool) string {
	found := ""
	for _, r := range rules {
		words := r.validate
		if sdk {
			words = r.sdk
		}
		if !strings.Contains(text, words) {
			continue
		}
		if found != "" {
			return "more than one: " + found + ", " + r.name
		}
		found = r.name
	}
	if found == "" {
		return "none: " + text
	}
	return found
}

// Names under five characters cannot derive at all, so Validate refuses them
// at construction rather than leaving the first mint to find out. The name
// is measured after the SDK trims it.
func TestValidateRefusesShortNames(t *testing.T) {
	for _, name := range []string{"", "a", "ab", "abc", "abcd", "  abcd  ", "ABCD", "     "} {
		d := derivation(wallet.SecurityLevelEveryApp, name, "entry")
		err := d.Validate()
		if !errors.Is(err, pushdrop.ErrDerivation) || !strings.Contains(err.Error(), "under 5 characters") {
			t.Errorf("%q: %v", name, err)
		}
		if sdkDerives(d) == nil {
			t.Errorf("%q: the SDK derives it, so Validate is stricter than the SDK", name)
		}
	}
	if err := derivation(wallet.SecurityLevelEveryApp, "abcde", "entry").Validate(); err != nil {
		t.Errorf("a five-character name: %v", err)
	}
}

// Validate answers as the SDK does on every rule the SDK applies, including
// the boundaries of each, so it never refuses a derivation the SDK would make
// and never passes one the SDK would refuse.
func TestValidateAgreesWithSDK(t *testing.T) {
	every := wallet.SecurityLevelEveryApp
	linkage := "specific linkage revelation "
	for _, tc := range []struct {
		name string
		d    pushdrop.Derivation
		ok   bool
	}{
		{"neutral", sample, true},
		{"security level 0", derivation(wallet.SecurityLevelSilent, "sample", "entry"), true},
		{"security level 2", derivation(wallet.SecurityLevelEveryAppAndCounterparty, "sample", "entry"), true},
		{"security level 3", derivation(3, "sample", "entry"), false},
		{"security level -1", derivation(-1, "sample", "entry"), false},
		{"empty key id", derivation(every, "sample", ""), false},
		{"one-byte key id", derivation(every, "sample", "x"), true},
		{"800-byte key id", derivation(every, "sample", strings.Repeat("k", 800)), true},
		{"801-byte key id", derivation(every, "sample", strings.Repeat("k", 801)), false},
		{"upper case", derivation(every, "Sample", "entry"), true},
		{"padded", derivation(every, "  sample  ", "entry"), true},
		{"inner space", derivation(every, "sample app", "entry"), true},
		{"digits", derivation(every, "sample 2", "entry"), true},
		{"two spaces", derivation(every, "sample  app", "entry"), false},
		{"hyphen", derivation(every, "sample-app", "entry"), false},
		{"underscore", derivation(every, "sample_app", "entry"), false},
		{"non-ASCII", derivation(every, "samplé", "entry"), false},
		{"protocol suffix", derivation(every, "sample protocol", "entry"), false},
		{"protocol inside", derivation(every, "protocol sample", "entry"), true},
		{"400-byte name", derivation(every, strings.Repeat("n", 400), "entry"), true},
		{"401-byte name", derivation(every, strings.Repeat("n", 401), "entry"), false},
		{"401-byte linkage name", derivation(every, linkage+strings.Repeat("n", 401-len(linkage)), "entry"), true},
		{"430-byte linkage name", derivation(every, linkage+strings.Repeat("n", 430-len(linkage)), "entry"), true},
		{"431-byte linkage name", derivation(every, linkage+strings.Repeat("n", 431-len(linkage)), "entry"), false},
	} {
		err, sdkErr := tc.d.Validate(), sdkDerives(tc.d)
		if (err == nil) != tc.ok {
			t.Errorf("%s: Validate says %v", tc.name, err)
		}
		if err != nil && !errors.Is(err, pushdrop.ErrDerivation) {
			t.Errorf("%s: %v is not ErrDerivation", tc.name, err)
		}
		if (sdkErr == nil) != tc.ok {
			t.Errorf("%s: the SDK says %v, so the row is wrong", tc.name, sdkErr)
		}
		if err != nil && sdkErr != nil && ruleOf(err.Error(), false) != ruleOf(sdkErr.Error(), true) {
			t.Errorf("%s: Validate says %q, the SDK %q", tc.name, err, sdkErr)
		}
	}
}

// A derivation that breaks two rules is refused by the one the SDK checks
// first, so Validate's refusal names the rule the SDK's own error names. Each
// row breaks two rules next to each other in the SDK's order; a Validate that
// checked them the other way round would name the later one.
func TestValidateReportsTheSDKsRule(t *testing.T) {
	every := wallet.SecurityLevelEveryApp
	linkage := "specific linkage revelation "
	long := strings.Repeat("k", 801)
	for _, tc := range []struct {
		name string
		d    pushdrop.Derivation
		rule string
	}{
		{"level and long key id", derivation(3, "sample", long), "security level"},
		{"level and empty key id", derivation(3, "sample", ""), "security level"},
		{"level and short name", derivation(-1, "abcd", "entry"), "security level"},
		{"long key id and short name", derivation(every, "abcd", long), "key id too long"},
		{"empty key id and short name", derivation(every, "abcd", ""), "empty key id"},
		{"empty key id and long name", derivation(every, strings.Repeat("n", 401), ""), "empty key id"},
		{"long name and a hyphen", derivation(every, strings.Repeat("n", 400)+"-", "entry"), "name too long"},
		{"long linkage name and a hyphen", derivation(every, linkage+strings.Repeat("n", 430-len(linkage))+"-", "entry"), "linkage name too long"},
		{"short name and two spaces", derivation(every, "a  b", "entry"), "name too short"},
		{"short name and a hyphen", derivation(every, "ab-c", "entry"), "name too short"},
		{"two spaces and a hyphen", derivation(every, "sample  app-x", "entry"), "consecutive spaces"},
		{"two spaces and the suffix", derivation(every, "sample  x protocol", "entry"), "consecutive spaces"},
		{"a hyphen and the suffix", derivation(every, "sample-x protocol", "entry"), "characters"},
	} {
		err, sdkErr := tc.d.Validate(), sdkDerives(tc.d)
		if err == nil || sdkErr == nil {
			t.Errorf("%s: Validate says %v, the SDK %v; both must refuse", tc.name, err, sdkErr)
			continue
		}
		if got := ruleOf(sdkErr.Error(), true); got != tc.rule {
			t.Errorf("%s: the SDK names %q (%v), so the row is wrong", tc.name, got, sdkErr)
		}
		if got := ruleOf(err.Error(), false); got != tc.rule {
			t.Errorf("%s: Validate names %q (%v), the SDK %q", tc.name, got, err, tc.rule)
		}
	}
}
