package headers

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Kind names the dialect a header source speaks.
type Kind int

const (
	// Native is an overlay bridge's /v1 routes: a root per height and a tip,
	// with no header fields, so no proof of work can be checked.
	Native Kind = iota
	// WhatsOnChain is the public WhatsOnChain API for one network.
	WhatsOnChain
	// Chaintracks is a chaintracks v2 service (GET /height and
	// GET /header/height/{h} under the base).
	Chaintracks
	// BlockHeadersService is a block-headers-service (bsv-blockchain)
	// API: GET /chain/tip/longest and GET /chain/header/byHeight under
	// /api/v1, with a bearer token when the service requires one.
	BlockHeadersService
	// Arcade is the chaintracks server an arcade installation embeds,
	// under /chaintracks/v2: GET /height and GET /header/height/{h}, each
	// answering the value bare rather than in a status envelope.
	Arcade
)

func (k Kind) String() string {
	switch k {
	case WhatsOnChain:
		return "whatsonchain"
	case Chaintracks:
		return "chaintracks"
	case BlockHeadersService:
		return "block-headers-service"
	case Arcade:
		return "arcade"
	default:
		return "native"
	}
}

// Networks a source may be checked against. The network decides the
// proof-of-work floor, nothing else.
const (
	Mainnet = "main"
	Testnet = "test"
	Regtest = "regtest"
)

// MainnetMinDifficulty is the lowest difficulty a mainnet header may claim.
//
// Every mainnet block since the 2018 split has carried a difficulty between
// about 2.6e10 and 5.2e11 (sampled across that range in 2026). The floor sits
// well below the lowest of those, so a real header never trips it, and well
// above what anyone could mine for the price of a lie: a header at 4e9 takes
// about 1.7e19 hashes. Without a floor a lying source would hand back a
// header whose bits claim a target nobody needs to work for, and the header
// would pass its own proof-of-work check.
const MainnetMinDifficulty = 4e9

// ErrSource is a header source specification that cannot be used.
var ErrSource = errors.New("headers: bad header source")

const wocBase = "https://api.whatsonchain.com/v1/bsv/"

// Parse reads a header source specification:
//
//	woc:main | woc:test          the public WhatsOnChain API
//	chaintracks:https://host/v2  a chaintracks v2 service
//	bhs:https://host:8080        a block-headers-service (/api/v1 is added
//	                             to a URL with no path); set Client.Token
//	                             when it requires one
//	arcade:https://host          an arcade installation's embedded
//	                             chaintracks server (/chaintracks/v2)
//	https://host:port            an overlay bridge's native /v1 routes
//
// and reports the kind, the base URL and the network the kind implies ("" for
// a native source, which carries no header fields to check). chaintracks,
// bhs and arcade imply mainnet; set Client.Network for a testnet one.
func Parse(spec string) (Kind, string, string, error) {
	s := strings.TrimSpace(spec)
	switch {
	case s == "":
		return 0, "", "", fmt.Errorf("%w: empty", ErrSource)
	case strings.HasPrefix(s, "woc:"):
		net := strings.TrimPrefix(s, "woc:")
		if net != Mainnet && net != Testnet {
			return 0, "", "", fmt.Errorf("%w: %q: WhatsOnChain serves woc:main or woc:test", ErrSource, spec)
		}
		return WhatsOnChain, wocBase + net, net, nil
	case strings.HasPrefix(s, "chaintracks:"):
		base := strings.TrimRight(strings.TrimPrefix(s, "chaintracks:"), "/")
		if err := httpURL(base); err != nil {
			return 0, "", "", fmt.Errorf("%w: %q: %v", ErrSource, spec, err)
		}
		return Chaintracks, base, Mainnet, nil
	case strings.HasPrefix(s, "bhs:"):
		base, err := withPath(strings.TrimPrefix(s, "bhs:"), "/api/v1")
		if err != nil {
			return 0, "", "", fmt.Errorf("%w: %q: %v", ErrSource, spec, err)
		}
		return BlockHeadersService, base, Mainnet, nil
	case strings.HasPrefix(s, "arcade:"):
		base := strings.TrimRight(strings.TrimPrefix(s, "arcade:"), "/")
		if err := httpURL(base); err != nil {
			return 0, "", "", fmt.Errorf("%w: %q: %v", ErrSource, spec, err)
		}
		return Arcade, base + "/chaintracks/v2", Mainnet, nil
	default:
		if err := httpURL(s); err != nil {
			return 0, "", "", fmt.Errorf("%w: %q: %v (use woc:main, chaintracks:URL, bhs:URL, arcade:URL or a bridge URL)", ErrSource, spec, err)
		}
		return Native, strings.TrimRight(s, "/"), "", nil
	}
}

// withPath is s with def as its path when it has none, trailing slashes
// dropped.
func withPath(s, def string) (string, error) {
	if err := httpURL(s); err != nil {
		return "", err
	}
	u, _ := url.Parse(s)
	if strings.Trim(u.Path, "/") == "" {
		u.Path = def
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func httpURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("not an http or https URL")
	}
	return nil
}

// NewSource returns a client for a header source specification (see Parse).
// A source with header fields is checked for proof of work at the floor of
// the network it implies; set Network after construction to check it against
// another.
func NewSource(spec string) (*Client, error) {
	kind, base, network, err := Parse(spec)
	if err != nil {
		return nil, err
	}
	return &Client{Base: base, Kind: kind, Network: network}, nil
}
