package termsroute

import (
	"errors"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/auth"
)

// DefaultMaxSessions is the default bound on BRC-104 sessions the terms
// route keeps, and DefaultSessionTTL their idle life in seconds.
const (
	DefaultMaxSessions = 10_000
	DefaultSessionTTL  = 600
)

type sessionEntry struct {
	s  *auth.PeerSession
	at time.Time
}

// Sessions is the BRC-104 sessions the terms route keeps, bounded. The
// SDK's own store keeps every session for the life of the process, and a
// handshake is unauthenticated, so anyone could grow it without end. This
// one holds at most max sessions and forgets one idle for ttl (idle: not
// added, updated or looked up); past the cap it forgets the least recently
// used. A client whose session was forgotten is refused (401) and shakes
// hands again. It is the SessionManager contract the SDK's Peer uses.
type Sessions struct {
	max     int
	ttl     time.Duration
	now     func() time.Time
	evicted func(why string)

	mu sync.Mutex
	// order is by session nonce, least recently used first.
	order []string
	byN   map[string]*sessionEntry
	byKey map[string]map[string]bool
}

var _ auth.SessionManager = (*Sessions)(nil)

// NewSessions makes a bounded store. evicted, when set, is told why each
// forgotten session went: "cap" or "idle".
func NewSessions(max int, ttl time.Duration, now func() time.Time, evicted func(why string)) (*Sessions, error) {
	if max < 1 {
		return nil, errors.New("Sessions: max must be at least 1")
	}
	if ttl <= 0 {
		return nil, errors.New("Sessions: ttl must be positive")
	}
	if now == nil {
		now = time.Now
	}
	if evicted == nil {
		evicted = func(string) {}
	}
	return &Sessions{max: max, ttl: ttl, now: now, evicted: evicted, byN: map[string]*sessionEntry{}, byKey: map[string]map[string]bool{}}, nil
}

// Size is how many sessions are kept.
func (s *Sessions) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.order)
}

func (s *Sessions) forget(nonce string) {
	e, ok := s.byN[nonce]
	if !ok {
		return
	}
	delete(s.byN, nonce)
	for i, n := range s.order {
		if n == nonce {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	if e.s.PeerIdentityKey != nil {
		k := e.s.PeerIdentityKey.ToDERHex()
		delete(s.byKey[k], nonce)
		if len(s.byKey[k]) == 0 {
			delete(s.byKey, k)
		}
	}
}

func (s *Sessions) put(nonce string, e *sessionEntry) {
	s.byN[nonce] = e
	s.order = append(s.order, nonce)
}

// expire forgets every session idle for ttl: the oldest come first.
func (s *Sessions) expire() {
	cutoff := s.now().Add(-s.ttl)
	for len(s.order) > 0 {
		e := s.byN[s.order[0]]
		if e.at.After(cutoff) {
			break
		}
		s.forget(s.order[0])
		s.evicted("idle")
	}
}

func (s *Sessions) prune() {
	s.expire()
	for len(s.order) > s.max {
		s.forget(s.order[0])
		s.evicted("cap")
	}
}

// AddSession implements auth.SessionManager.
func (s *Sessions) AddSession(session *auth.PeerSession) error {
	if session == nil || session.SessionNonce == "" {
		return errors.New("invalid session: sessionNonce is required to add a session")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(session.SessionNonce)
	s.put(session.SessionNonce, &sessionEntry{s: session, at: s.now()})
	if session.PeerIdentityKey != nil {
		k := session.PeerIdentityKey.ToDERHex()
		if s.byKey[k] == nil {
			s.byKey[k] = map[string]bool{}
		}
		s.byKey[k][session.SessionNonce] = true
	}
	s.prune()
	return nil
}

// UpdateSession implements auth.SessionManager.
func (s *Sessions) UpdateSession(session *auth.PeerSession) { _ = s.AddSession(session) }

func (s *Sessions) touch(nonce string, e *sessionEntry) *auth.PeerSession {
	s.forget(nonce)
	s.put(nonce, &sessionEntry{s: e.s, at: s.now()})
	if e.s.PeerIdentityKey != nil {
		k := e.s.PeerIdentityKey.ToDERHex()
		if s.byKey[k] == nil {
			s.byKey[k] = map[string]bool{}
		}
		s.byKey[k][nonce] = true
	}
	return e.s
}

// GetSession implements auth.SessionManager: by session nonce, or else the
// most recently updated session of an identity key.
func (s *Sessions) GetSession(identifier string) (*auth.PeerSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire()
	if e, ok := s.byN[identifier]; ok {
		return s.touch(identifier, e), nil
	}
	var best *sessionEntry
	var bestNonce string
	for nonce := range s.byKey[identifier] {
		e, ok := s.byN[nonce]
		if !ok {
			continue
		}
		if best == nil || e.s.LoadLastUpdate() > best.s.LoadLastUpdate() {
			best, bestNonce = e, nonce
		}
	}
	if best == nil {
		return nil, errors.New("session-not-found")
	}
	return s.touch(bestNonce, best), nil
}

// RemoveSession implements auth.SessionManager.
func (s *Sessions) RemoveSession(session *auth.PeerSession) {
	if session == nil || session.SessionNonce == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forget(session.SessionNonce)
}

// HasSession implements auth.SessionManager.
func (s *Sessions) HasSession(identifier string) bool {
	_, err := s.GetSession(identifier)
	return err == nil
}
