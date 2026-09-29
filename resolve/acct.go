package resolve

import (
	"fmt"
	"strings"
)

// Acct is a parsed recipient: the fully qualified form of BRC-169 section 2.1
// with the ecosystem already known to be a domain.
type Acct struct {
	// Handle is lowercase, with any +tag stripped.
	Handle string
	// Domain is the lowercase FQDN. Never an alias; see ParseAcct.
	Domain string
	// Tag is the "+tag" subhandle if one was given, without the "+". It is
	// kept because it is the recipient's routing hint (section 3) and
	// stripped from Handle because resolution is on the bare handle (5.2).
	Tag string
}

// String returns handle@domain, the paymail-style form, which is how an
// address is written on a command line and keyed in a pin file.
func (a Acct) String() string { return a.Handle + "@" + a.Domain }

const (
	maxHandle = 64
	maxTag    = 32
	maxDomain = 253
	maxLabel  = 63
)

// ParseAcct accepts user@domain, @user@domain and acct:user@domain, lowercases
// per section 2.1 rule 2, strips and keeps a +tag, and validates the grammar.
//
// The ecosystem MUST be a domain. Section 2.1 rule 5 makes the dot the
// discriminator, so a dotless ecosystem is an alias, and aliases are not
// supported here: resolving one means trusting a directory to say which
// domain "example" is, which is a trust step this package has no way to check.
// The error says so rather than guessing at a hostname the user did not type.
func ParseAcct(s string) (Acct, error) {
	in := s
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "acct:")
	s = strings.TrimPrefix(s, "@")
	handle, domain, ok := strings.Cut(s, "@")
	if !ok || domain == "" {
		return Acct{}, fmt.Errorf("resolve: %q: no @domain; the ecosystem must be named in full as handle@domain.tld", in)
	}
	if strings.Contains(domain, "@") {
		return Acct{}, fmt.Errorf("resolve: %q: more than one @ separates handle and domain", in)
	}
	handle, tag, hasTag := strings.Cut(handle, "+")
	if err := checkLabel("handle", handle, maxHandle); err != nil {
		return Acct{}, fmt.Errorf("resolve: %q: %w", in, err)
	}
	if hasTag {
		if err := checkLabel("tag", tag, maxTag); err != nil {
			return Acct{}, fmt.Errorf("resolve: %q: %w", in, err)
		}
	}
	if !strings.Contains(domain, ".") {
		return Acct{}, fmt.Errorf("resolve: %q: ecosystem %q has no dot, so it is an alias (BRC-169 section 2.1 rule 5); aliases are not supported, name the domain in full", in, domain)
	}
	if err := checkDomain(domain); err != nil {
		return Acct{}, fmt.Errorf("resolve: %q: %w", in, err)
	}
	return Acct{Handle: handle, Domain: domain, Tag: tag}, nil
}

func isAlnum(c byte) bool { return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' }

// checkLabel enforces the handle and tag grammar: 1 to limit characters of
// lowercase alphanumerics, with "." "_" "-" allowed only between them.
func checkLabel(kind, s string, limit int) error {
	if s == "" {
		return fmt.Errorf("%s is empty", kind)
	}
	if len(s) > limit {
		return fmt.Errorf("%s is %d characters, the limit is %d", kind, len(s), limit)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isAlnum(c):
		case c == '.' || c == '_' || c == '-':
			if i == 0 || i == len(s)-1 {
				return fmt.Errorf("%s %q must begin and end with a letter or digit", kind, s)
			}
		default:
			return fmt.Errorf("%s %q contains %q; only a-z, 0-9 and internal . _ - are allowed", kind, s, string(c))
		}
	}
	return nil
}

// checkDomain enforces the RFC 1123 section 2.1 hostname shape on an already
// lowercased domain. A port, a path or a scheme is refused with the rest: the
// grammar names a host, and anything else is not one.
func checkDomain(d string) error {
	if len(d) > maxDomain {
		return fmt.Errorf("domain is %d characters, the limit is %d", len(d), maxDomain)
	}
	for _, label := range strings.Split(d, ".") {
		if label == "" {
			return fmt.Errorf("domain %q has an empty label", d)
		}
		if len(label) > maxLabel {
			return fmt.Errorf("domain %q: label %q exceeds %d characters", d, label, maxLabel)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("domain %q: label %q must not begin or end with a hyphen", d, label)
		}
		for i := 0; i < len(label); i++ {
			if c := label[i]; !isAlnum(c) && c != '-' {
				return fmt.Errorf("domain %q contains %q; only a-z, 0-9, - and . are allowed", d, string(c))
			}
		}
	}
	return nil
}
