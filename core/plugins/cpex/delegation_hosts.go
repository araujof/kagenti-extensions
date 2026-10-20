package cpex

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/gobwas/glob"

	"github.com/rossoctl/cortex/core/internal/hostglob"
)

// codeDelegationDestinationMismatch is the violation code for a request
// whose delegate() step minted a credential its destination host is not
// bound to in delegated_credential_hosts.
const codeDelegationDestinationMismatch = "delegation_destination_mismatch"

// credentialHosts binds each delegation target to the hosts its
// credentials may be sent to. Patterns are `.`-delimited globs, as in
// skip_hosts and authproxy-routes, matched against the host without its
// port.
type credentialHosts map[string][]*glob.Pattern

// hostPatterns is a compiled list of `.`-delimited host globs, matched
// as credentialHosts matches them.
type hostPatterns []*glob.Pattern

// newHostPatterns compiles the patterns of the config key name. A pattern
// that is empty, carries a port, or matches every in-cluster host is
// refused.
func newHostPatterns(name string, patterns []string) (hostPatterns, error) {
	var out hostPatterns
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		switch {
		case p == "":
			return nil, fmt.Errorf("%s: empty pattern", name)
		case strings.Contains(p, ":"):
			return nil, fmt.Errorf("%s: pattern %q must not carry a port", name, p)
		}
		g, err := hostglob.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid pattern %q: %w", name, p, err)
		}
		if hostglob.MatchesEverySingleLabel(g) {
			return nil, fmt.Errorf("%s: pattern %q matches every host", name, p)
		}
		out = append(out, g)
	}
	return out, nil
}

// match reports whether host, without its port, matches a pattern.
func (h hostPatterns) match(host string) bool {
	if host == "" {
		return false
	}
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, g := range h {
		if g.Match(host) {
			return true
		}
	}
	return false
}

// newCredentialHosts compiles delegated_credential_hosts. Every target
// needs at least one pattern; a pattern that matches every in-cluster
// host, or carries a port, is refused.
func newCredentialHosts(m map[string][]string) (credentialHosts, error) {
	if len(m) == 0 {
		return nil, nil
	}
	targets := make([]string, 0, len(m))
	for t := range m {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	out := make(credentialHosts, len(m))
	for _, target := range targets {
		patterns := m[target]
		if strings.TrimSpace(target) == "" {
			return nil, fmt.Errorf("delegated_credential_hosts: empty target")
		}
		if len(patterns) == 0 {
			return nil, fmt.Errorf("delegated_credential_hosts: target %q names no hosts", target)
		}
		for _, p := range patterns {
			p = strings.ToLower(strings.TrimSpace(p))
			switch {
			case p == "":
				return nil, fmt.Errorf("delegated_credential_hosts: target %q: empty pattern", target)
			case strings.Contains(p, ":"):
				return nil, fmt.Errorf("delegated_credential_hosts: target %q: pattern %q must not carry a port", target, p)
			}
			g, err := hostglob.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("delegated_credential_hosts: target %q: invalid pattern %q: %w", target, p, err)
			}
			if hostglob.MatchesEverySingleLabel(g) {
				return nil, fmt.Errorf("delegated_credential_hosts: target %q: pattern %q matches every host", target, p)
			}
			out[target] = append(out[target], g)
		}
	}
	return out, nil
}

// allows reports whether a credential for target may be sent to host.
func (h credentialHosts) allows(target, host string) bool {
	if target == "" || host == "" {
		return false
	}
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, g := range h[target] {
		if g.Match(host) {
			return true
		}
	}
	return false
}

// credentialTarget is the key a delegated credential is bound by: the
// target its delegate() step named, else its audience.
func credentialTarget(target, audience string) string {
	if target != "" {
		return target
	}
	return audience
}
