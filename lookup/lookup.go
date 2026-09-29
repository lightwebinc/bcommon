// Package lookup asks an overlay host a BRC-24 question and returns its
// output-list answer. The question itself (which service, what query) is the
// caller's.
//
// It is plain net/http over hostset on purpose. go-sdk ships a
// lookup.LookupResolver, and it is not used here because, unconfigured, it
// runs SLAP discovery against hard-coded public trackers; a client that
// dialed the public internet to find a host the domain already named would
// break BRC-180's "contact no host the user did not name". The host to ask is
// the one the caller names, from a manifest or its own configuration, and the
// only open question is which of its addresses answers, which is hostset's
// job.
//
// The answer shape is the one the TypeScript overlay host actually emits,
// pinned by a captured fixture rather than by the SDK's types: it encodes
// beef as a JSON array of numbers, where the SDK's OutputListItem expects
// base64. Output accepts both.
package lookup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/lightwebinc/bcommon/hostset"
)

// Output is one item of an output-list answer.
type Output struct {
	Beef        []byte
	OutputIndex uint32
	Context     []byte
}

// UnmarshalJSON accepts beef and context as either a JSON array of byte
// values (what the TypeScript host serialises a number[] to) or a base64
// string (what encoding/json and go-sdk produce for []byte). Refusing either
// form would refuse a conforming host; the fixture test proves the array form
// is the live one.
func (o *Output) UnmarshalJSON(b []byte) error {
	var raw struct {
		Beef        json.RawMessage `json:"beef"`
		OutputIndex uint32          `json:"outputIndex"`
		Context     json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	beef, err := bytesFromJSON(raw.Beef)
	if err != nil {
		return fmt.Errorf("beef: %w", err)
	}
	ctx, err := bytesFromJSON(raw.Context)
	if err != nil {
		return fmt.Errorf("context: %w", err)
	}
	o.Beef, o.OutputIndex, o.Context = beef, raw.OutputIndex, ctx
	return nil
}

func bytesFromJSON(raw json.RawMessage) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	switch raw[0] {
	case '[':
		var nums []int64
		if err := json.Unmarshal(raw, &nums); err != nil {
			return nil, err
		}
		out := make([]byte, len(nums))
		for i, n := range nums {
			if n < 0 || n > 255 {
				return nil, fmt.Errorf("element %d is %d, not a byte", i, n)
			}
			out[i] = byte(n)
		}
		return out, nil
	case '"':
		var s []byte
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, errors.New("neither an array of bytes nor a base64 string")
}

// TypeOutputList is the one answer type Query accepts.
const TypeOutputList = "output-list"

// Answer is a BRC-24 lookup answer.
type Answer struct {
	Type    string   `json:"type"`
	Outputs []Output `json:"outputs"`
}

// Question is the body of POST /lookup. Query is marshalled as it is, so
// its JSON shape is whatever the lookup service parses.
type Question struct {
	Service string `json:"service"`
	Query   any    `json:"query"`
}

// HostAnswer is one host's answer, kept with the host so the caller can name
// the replica that disagreed.
type HostAnswer struct {
	Host   hostset.Host
	Answer Answer
}

// ErrNotOutputList refuses an answer of any other type.
var ErrNotOutputList = errors.New("lookup: answer is not an output-list")

// Query POSTs q to <base>/lookup on the hosts hs selects and decodes each
// answer. Every host that answered must have answered 200 with an output-list;
// one that answered anything else is an error naming it, not a skipped host,
// because hostset has already decided that host was up and the caller must
// not quietly lose an opinion it asked for.
//
// A nil hs asks the addresses the base URL's name resolves to, first success
// wins.
func Query(ctx context.Context, hs *hostset.Client, base string, q Question) ([]HostAnswer, error) {
	body, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("lookup: %w", err)
	}
	if hs == nil {
		hs = &hostset.Client{}
	}
	results, err := hs.Do(ctx, base, func(h hostset.Host) (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, strings.TrimRight(h.Base, "/")+"/lookup", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	var out []HostAnswer
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		if r.Status != http.StatusOK {
			// The host's own words travel with the status: "Lookup service
			// not found" is a different problem from a bad question, and the
			// operator should not have to reproduce the request to tell.
			return nil, fmt.Errorf("lookup: %s@%s answered status %d: %s", r.Host.Name, r.Host.Addr, r.Status, snippet(r.Body))
		}
		var a Answer
		if err := json.Unmarshal(r.Body, &a); err != nil {
			return nil, fmt.Errorf("lookup: %s@%s: answer is not the expected JSON: %w", r.Host.Name, r.Host.Addr, err)
		}
		if a.Type != TypeOutputList {
			return nil, fmt.Errorf("%w: %s@%s answered type %q", ErrNotOutputList, r.Host.Name, r.Host.Addr, a.Type)
		}
		out = append(out, HostAnswer{Host: r.Host, Answer: a})
	}
	return out, nil
}

// snippet trims a host's error body for an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}
