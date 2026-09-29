package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Manifest is the part of a domain's /manifest.json this package reads: the
// BRC-180 overlay map, the BRC-169 handles object and the BRC-68 trust anchor.
// Unknown keys are ignored, as both specifications require, and Raw keeps the
// whole document for a caller that needs one this struct does not carry.
type Manifest struct {
	// Overlays maps a topic manager or lookup service name to its base URL
	// (BRC-180). Nil when the domain declares no overlays, which is a valid
	// manifest and not an error.
	Overlays map[string]string
	// Handles is the metanet.handles object (BRC-169 section 5.1). Nil when
	// the domain does not offer handle resolution.
	Handles *Handles
	// Trust is the metanet.trust anchor (BRC-68). Its PublicKey is the
	// certifier key handle certificates are checked against.
	Trust *Trust
	// Raw is the document as served.
	Raw json.RawMessage
}

// Handles is metanet.handles.
type Handles struct {
	Version    string   `json:"version"`
	Resolve    string   `json:"resolve"`
	Search     string   `json:"search"`
	Reverse    string   `json:"reverse"`
	Messagebox string   `json:"messagebox"`
	Aliases    []string `json:"aliases"`
}

// Trust is metanet.trust.
type Trust struct {
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
}

// Overlay returns the base URL the domain declares for a service. ok is false
// when the entry is absent, and BRC-180 is explicit about what that means: the
// domain does not offer the service, and the client must not go looking.
func (m *Manifest) Overlay(name string) (string, bool) {
	if m == nil {
		return "", false
	}
	base, ok := m.Overlays[name]
	return base, ok && base != ""
}

// ParseManifest decodes a manifest document.
func ParseManifest(body []byte) (*Manifest, error) {
	var doc struct {
		Metanet struct {
			Overlays map[string]string `json:"overlays"`
			Handles  *Handles          `json:"handles"`
			Trust    *Trust            `json:"trust"`
		} `json:"metanet"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("manifest is not the expected JSON: %w", err)
	}
	return &Manifest{
		Overlays: doc.Metanet.Overlays,
		Handles:  doc.Metanet.Handles,
		Trust:    doc.Metanet.Trust,
		Raw:      append(json.RawMessage(nil), body...),
	}, nil
}

// FetchManifest reads https://<domain>/manifest.json.
//
// The path is fixed by BRC-68 and the scheme by BRC-180; there is no fallback
// to http and no alternative location, because a manifest reached any other
// way would not be secured by control of the domain, which is the only thing
// securing it.
func FetchManifest(ctx context.Context, c *http.Client, domain string) (*Manifest, error) {
	if domain == "" || strings.ContainsAny(domain, "/?#@ ") {
		return nil, fmt.Errorf("resolve: %q is not a domain", domain)
	}
	u := &url.URL{Scheme: "https", Host: domain, Path: "/manifest.json"}
	status, body, err := get(ctx, c, u)
	if err != nil {
		return nil, fmt.Errorf("resolve: manifest for %s: %w", domain, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("resolve: manifest for %s: status %d", domain, status)
	}
	m, err := ParseManifest(body)
	if err != nil {
		return nil, fmt.Errorf("resolve: manifest for %s: %w", domain, err)
	}
	return m, nil
}
