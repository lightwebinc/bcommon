package termsroute

import (
	"errors"
	"math"
	"net"
	"strings"
	"sync"
	"time"
)

// BudgetConfig is the handshake budget: token buckets, one for the route
// and one for each remote address, checked before any signature work.
type BudgetConfig struct {
	// PerSec is the handshakes a second the route answers, Burst the most
	// it answers at once.
	PerSec float64
	Burst  int
	// The same for each remote address.
	PerAddressPerSec float64
	AddressBurst     int
}

// DefaultBudget is a fraction of the roughly 10 handshakes a second a test
// host's process signs, so its own work keeps most of the process. An
// honest client shakes hands once per session.
var DefaultBudget = BudgetConfig{PerSec: 4, Burst: 8, PerAddressPerSec: 1, AddressBurst: 4}

// MaxAddresses is the most addresses with a bucket; past it the least
// recently seen is forgotten.
const MaxAddresses = 10_000

type bucket struct {
	tokens float64
	at     time.Time
}

func (b *bucket) refill(rate float64, burst int, now time.Time) {
	if now.After(b.at) {
		b.tokens = math.Min(float64(burst), b.tokens+now.Sub(b.at).Seconds()*rate)
	}
	b.at = now
}

// wait is the seconds until one token is there, at least 1.
func (b *bucket) wait(rate float64) int {
	return max(1, int(math.Ceil((1-b.tokens)/rate)))
}

// Verdict is a budget's answer: OK, or the limit that refused and how long
// to wait, in seconds.
type Verdict struct {
	OK         bool
	Limit      string // "address" or "global"
	RetryAfter int
}

// AddressKey is the key an address is budgeted under: IPv4 as is, an
// IPv4-mapped address as its IPv4 address, IPv6 by its /64.
func AddressKey(address string) string {
	a := strings.ToLower(strings.TrimSpace(address))
	if h, _, err := net.SplitHostPort(a); err == nil {
		a = h
	}
	if i := strings.IndexByte(a, '%'); i >= 0 {
		a = a[:i]
	}
	ip := net.ParseIP(a)
	if ip == nil {
		return a
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	masked := ip.Mask(net.CIDRMask(64, 128))
	return masked.String() + "/64"
}

type keyed struct {
	key string
	b   *bucket
}

// HandshakeBudget answers handshakes from the route's bucket and each
// address's, the address's checked first, so one address that floods
// spends its own budget and not the route's.
type HandshakeBudget struct {
	c   BudgetConfig
	now func() time.Time

	mu     sync.Mutex
	global bucket
	// byAddress is least recently seen first.
	order []keyed
	index map[string]int
}

// NewHandshakeBudget makes a budget; every rate and burst must be positive.
func NewHandshakeBudget(c BudgetConfig, now func() time.Time) (*HandshakeBudget, error) {
	if now == nil {
		now = time.Now
	}
	if !(c.PerSec > 0) || !(c.PerAddressPerSec > 0) || c.Burst < 1 || c.AddressBurst < 1 {
		return nil, errors.New("HandshakeBudget: every rate must be positive and every burst at least 1")
	}
	return &HandshakeBudget{c: c, now: now, global: bucket{tokens: float64(c.Burst), at: now()}, index: map[string]int{}}, nil
}

// Addresses is how many addresses have a bucket.
func (h *HandshakeBudget) Addresses() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.order)
}

func (h *HandshakeBudget) remove(i int) {
	delete(h.index, h.order[i].key)
	h.order = append(h.order[:i], h.order[i+1:]...)
	for j := i; j < len(h.order); j++ {
		h.index[h.order[j].key] = j
	}
}

// Take takes one handshake for address from both buckets, or says which
// refused and how long to wait.
func (h *HandshakeBudget) Take(address string) Verdict {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	key := AddressKey(address)
	var a *bucket
	if i, ok := h.index[key]; ok {
		a = h.order[i].b
		h.remove(i)
	} else {
		a = &bucket{tokens: float64(h.c.AddressBurst), at: now}
	}
	// Forget addresses whose buckets refilled, oldest first, and any past
	// the cap, leaving room for one.
	for len(h.order) > 0 {
		if len(h.order) < MaxAddresses {
			b := h.order[0].b
			b.refill(h.c.PerAddressPerSec, h.c.AddressBurst, now)
			if b.tokens < float64(h.c.AddressBurst) {
				break
			}
		}
		h.remove(0)
	}
	h.index[key] = len(h.order)
	h.order = append(h.order, keyed{key, a})
	a.refill(h.c.PerAddressPerSec, h.c.AddressBurst, now)
	if a.tokens < 1 {
		return Verdict{Limit: "address", RetryAfter: a.wait(h.c.PerAddressPerSec)}
	}
	h.global.refill(h.c.PerSec, h.c.Burst, now)
	if h.global.tokens < 1 {
		return Verdict{Limit: "global", RetryAfter: h.global.wait(h.c.PerSec)}
	}
	a.tokens--
	h.global.tokens--
	return Verdict{OK: true}
}

// ResponseBudgetConfig is the budget of signed responses for each
// authenticated session: PerSec a second, Burst at once.
type ResponseBudgetConfig struct {
	PerSec float64
	Burst  int
}

// DefaultResponseBudget is far above what a reader walking pages needs and
// far below what a process signs.
var DefaultResponseBudget = ResponseBudgetConfig{PerSec: 5, Burst: 20}

// KeyedBudget is token buckets by key, at most max of them; past it the
// least recently used is forgotten.
type KeyedBudget struct {
	c   ResponseBudgetConfig
	max int
	now func() time.Time

	mu    sync.Mutex
	order []keyed
	index map[string]int
}

// NewKeyedBudget makes a budget of at most max keys.
func NewKeyedBudget(c ResponseBudgetConfig, max int, now func() time.Time) (*KeyedBudget, error) {
	if now == nil {
		now = time.Now
	}
	if !(c.PerSec > 0) || c.Burst < 1 || max < 1 {
		return nil, errors.New("KeyedBudget: perSec must be positive, burst and max at least 1")
	}
	return &KeyedBudget{c: c, max: max, now: now, index: map[string]int{}}, nil
}

// Size is how many keys have a bucket.
func (k *KeyedBudget) Size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.order)
}

func (k *KeyedBudget) remove(i int) {
	delete(k.index, k.order[i].key)
	k.order = append(k.order[:i], k.order[i+1:]...)
	for j := i; j < len(k.order); j++ {
		k.index[k.order[j].key] = j
	}
}

// Take takes one from key's bucket, or says how long until one is there.
func (k *KeyedBudget) Take(key string) Verdict {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	var b *bucket
	if i, ok := k.index[key]; ok {
		b = k.order[i].b
		k.remove(i)
	} else {
		b = &bucket{tokens: float64(k.c.Burst), at: now}
	}
	k.index[key] = len(k.order)
	k.order = append(k.order, keyed{key, b})
	for len(k.order) > k.max {
		k.remove(0)
	}
	b.refill(k.c.PerSec, k.c.Burst, now)
	if b.tokens < 1 {
		return Verdict{Limit: "session", RetryAfter: b.wait(k.c.PerSec)}
	}
	b.tokens--
	return Verdict{OK: true}
}

// MaxSeenRequests is the most request identifiers remembered, to refuse
// one sent twice.
const MaxSeenRequests = 65536

// SeenRequests is the requests a route answered lately, by a key of the
// asker and the request's own identifier: a request sent again byte for
// byte is refused without a signature. Bounded, oldest forgotten first;
// the response budget bounds what a replay older than this memory can
// cost.
type SeenRequests struct {
	max   int
	mu    sync.Mutex
	order []string
	seen  map[string]bool
}

// NewSeenRequests remembers at most max requests.
func NewSeenRequests(max int) *SeenRequests {
	if max < 1 {
		max = MaxSeenRequests
	}
	return &SeenRequests{max: max, seen: map[string]bool{}}
}

// Size is how many are remembered.
func (s *SeenRequests) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.order)
}

// Has reports whether key was answered lately.
func (s *SeenRequests) Has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen[key]
}

// Add remembers key.
func (s *SeenRequests) Add(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	s.order = append(s.order, key)
	for len(s.order) > s.max {
		delete(s.seen, s.order[0])
		s.order = s.order[1:]
	}
}
