// Package unicast publishes objects to overlay hosts one by one, for a
// deployment with no multicast plane.
//
// On the plane a publisher submits each object once, to one facade, and the
// plane delivers it to every subscribed host and repairs what is lost on the
// way. Without the plane the publisher does that itself: a Set submits every
// object to each of its hosts individually, retries each host on its own,
// and counts the object published once Need hosts took it. A host that
// missed it does not receive it from the others; the application's own
// fill or backfill copies it across later.
//
// A host "took" an object when it answered the submit with 200 and a STEAK
// for the topic. To a submitter a duplicate and a refusal look the same (an
// answer that admits nothing: a topic manager raises a refusal, and the
// engine does not return it as an error). Without a Confirm such an answer
// counts as taken: resending cannot change it. With a Confirm the
// application asks the host whether it answers the object, and only a yes
// counts.
package unicast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lightwebinc/bcommon/publish"
)

// Set is the hosts a publisher submits to.
type Set struct {
	// Hosts are the hosts' base URLs; each is POSTed <base>/submit.
	Hosts []string
	// Need is how many hosts must take an object for it to count.
	Need int
	// Retries are the waits between the tries of one host: a host is tried
	// 1+len(Retries) times before it is counted as missed.
	Retries []time.Duration
	// HTTP is the client each host's facade uses; nil is publish's default.
	HTTP *http.Client
	// Note receives a line per host that did not take an object, and with
	// Verbose a line per host that did.
	Note    func(format string, args ...any)
	Verbose bool

	mu    sync.Mutex
	tally map[string]*Tally
}

// Confirm asks host whether it answers the object, for an answer that
// admitted nothing. Nil counts such an answer as taken.
type Confirm func(ctx context.Context, host string) (bool, error)

// Result is one host's answer to one submit.
type Result struct {
	Host string
	// Result is the host's STEAK entry when Err is nil.
	Result publish.Result
	// Confirmed is set when the answer admitted nothing and a lookup at
	// the host answers the object.
	Confirmed bool
	// Err is the last failure when every try failed, or why an answer that
	// admitted nothing was not confirmed.
	Err error
	// Tries is how many times the host was asked.
	Tries int
}

// Took reports whether the host took the object.
func (r Result) Took() bool { return r.Err == nil }

// Outcome is every host's answer to one submit.
type Outcome struct {
	What    string
	Results []Result
	Need    int
}

// Took is how many hosts took the object.
func (o Outcome) Took() int {
	n := 0
	for _, r := range o.Results {
		if r.Took() {
			n++
		}
	}
	return n
}

// OK reports whether enough hosts took it.
func (o Outcome) OK() bool { return o.Took() >= o.Need }

// Err is nil when the outcome is OK, and otherwise names every host that
// missed the object and why.
func (o Outcome) Err() error {
	if o.OK() {
		return nil
	}
	var why []string
	for _, r := range o.Results {
		if !r.Took() {
			why = append(why, fmt.Sprintf("%s: %v", r.Host, r.Err))
		}
	}
	return fmt.Errorf("%d of %d host(s) took it and the quorum is %d (%s)", o.Took(), len(o.Results), o.Need, strings.Join(why, "; "))
}

// Missed are the hosts that did not take the object.
func (o Outcome) Missed() []string {
	var hs []string
	for _, r := range o.Results {
		if !r.Took() {
			hs = append(hs, r.Host)
		}
	}
	return hs
}

// Merged is one answer standing for every host's: the first that admitted
// outputs, else the first host's that answered at all, else the zero
// Result.
func (o Outcome) Merged() publish.Result {
	var first *publish.Result
	for i := range o.Results {
		r := &o.Results[i]
		if !r.Took() {
			continue
		}
		if len(r.Result.Admitted) > 0 {
			return r.Result
		}
		if first == nil {
			first = &r.Result
		}
	}
	if first == nil {
		return publish.Result{}
	}
	return *first
}

// Tally is what one host was sent in this process's lifetime.
type Tally struct {
	Host string
	// Took and Missed count the objects the host took and did not.
	Took, Missed int
	// MissedWhat names the missed objects, in order.
	MissedWhat []string
}

// Send submits beef for topic to every host at once, each retried on its
// own, and returns every host's answer. what names the object in notes.
// need is the quorum for this object; zero means s.Need. confirm, when
// set, is asked about every host whose answer admitted nothing.
func (s *Set) Send(ctx context.Context, what, topic string, beef []byte, need int, confirm Confirm) Outcome {
	if need == 0 {
		need = s.Need
	}
	o := Outcome{What: what, Results: make([]Result, len(s.Hosts)), Need: need}
	var wg sync.WaitGroup
	for i, h := range s.Hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o.Results[i] = s.one(ctx, h, topic, beef, confirm)
		}()
	}
	wg.Wait()
	s.record(o)
	return o
}

func (s *Set) one(ctx context.Context, host, topic string, beef []byte, confirm Confirm) Result {
	f := &publish.Facade{Base: host, HTTP: s.HTTP}
	r := Result{Host: host}
	for {
		r.Tries++
		res, err := f.Submit(ctx, topic, beef)
		if err == nil {
			r.Result, r.Err = res, nil
			if len(res.Admitted) == 0 && confirm != nil {
				ok, cerr := confirm(ctx, host)
				switch {
				case cerr != nil:
					r.Err = fmt.Errorf("admitted nothing, and the lookup to confirm it failed: %w", cerr)
				case !ok:
					r.Err = fmt.Errorf("admitted nothing, and a lookup does not answer it: the host refused it")
				default:
					r.Confirmed = true
				}
			}
			return r
		}
		r.Err = err
		if r.Tries > len(s.Retries) || ctx.Err() != nil {
			return r
		}
		select {
		case <-ctx.Done():
			r.Err = ctx.Err()
			return r
		case <-time.After(s.Retries[r.Tries-1]):
		}
	}
}

func (s *Set) note(format string, args ...any) {
	if s.Note != nil {
		s.Note(format, args...)
	}
}

func (s *Set) record(o Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tally == nil {
		s.tally = map[string]*Tally{}
	}
	for _, r := range o.Results {
		t := s.tally[r.Host]
		if t == nil {
			t = &Tally{Host: r.Host}
			s.tally[r.Host] = t
		}
		switch {
		case !r.Took():
			t.Missed++
			t.MissedWhat = append(t.MissedWhat, o.What)
			s.note("%s: host %s MISSED it after %d tries: %v", o.What, r.Host, r.Tries, r.Err)
		case s.Verbose && r.Confirmed:
			t.Took++
			s.note("%s: host %s holds it already", o.What, r.Host)
		case s.Verbose && len(r.Result.Admitted) > 0:
			t.Took++
			s.note("%s: host %s admitted %d output(s)", o.What, r.Host, len(r.Result.Admitted))
		case s.Verbose:
			t.Took++
			s.note("%s: host %s admitted nothing: it holds it already, or refused it", o.What, r.Host)
		default:
			t.Took++
		}
	}
}

// Tallies are the per-host counts so far, in Hosts order.
func (s *Set) Tallies() []Tally {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Tally, 0, len(s.Hosts))
	for _, h := range s.Hosts {
		if t := s.tally[h]; t != nil {
			c := *t
			c.MissedWhat = append([]string(nil), t.MissedWhat...)
			out = append(out, c)
		} else {
			out = append(out, Tally{Host: h})
		}
	}
	return out
}

// Facade is a publish.Facade that sends every Submit to each of the Set's
// hosts (Send, with no Confirm and the Set's quorum), for code that
// publishes through one facade, such as producer.Trees. Below the quorum
// it answers 502 naming the hosts that missed; otherwise the merged
// answer. what names the objects in notes and tallies.
func (s *Set) Facade(what string) *publish.Facade {
	return &publish.Facade{Base: "http://unicast.invalid", HTTP: &http.Client{Transport: fanout{s, what}}}
}

type fanout struct {
	s    *Set
	what string
}

func (f fanout) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	topic := req.Header.Get("x-topics")
	o := f.s.Send(req.Context(), f.what, topic, body, 0, nil)
	if err := o.Err(); err != nil {
		return respond(req, http.StatusBadGateway, []byte("unicast: "+err.Error())), nil
	}
	m := o.Merged()
	if len(m.Raw) > 0 {
		return respond(req, http.StatusOK, m.Raw), nil
	}
	raw, _ := json.Marshal(map[string]any{topic: map[string]any{"outputsToAdmit": m.Admitted, "coinsToRetain": m.Retained}})
	return respond(req, http.StatusOK, raw), nil
}

// Kept is a publish.Facade that sends nothing: every Submit is answered
// with nothing admitted. producer.Trees publishes each funding tree it
// adopts through one facade; an application whose funding trees are never
// published on their own (every carrier carries its own) gives it this.
func Kept() *publish.Facade {
	return &publish.Facade{Base: "http://kept.invalid", HTTP: &http.Client{Transport: kept{}}}
}

type kept struct{}

func (kept) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	raw, _ := json.Marshal(map[string]any{req.Header.Get("x-topics"): map[string]any{"outputsToAdmit": []int{}, "coinsToRetain": []int{}}})
	return respond(req, http.StatusOK, raw), nil
}

func respond(req *http.Request, code int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}
