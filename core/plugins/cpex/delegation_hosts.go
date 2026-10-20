package cpex

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
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

// hostPatterns is a compiled strict_hosts list: `.`-delimited host globs,
// matched as credentialHosts matches them, and IP prefixes, matched
// against a host that is an IP literal.
type hostPatterns struct {
	names    []*glob.Pattern
	prefixes []netip.Prefix
}

// newHostPatterns compiles the patterns of the config key name. Each is a
// host glob, an IP address ("10.96.0.10", "fd00::a", "[fd00::a]") or a
// CIDR ("10.96.0.0/12", "fd00:10:96::/112"). A pattern that is empty,
// carries a port or a zone, or matches every host or every address is
// refused.
func newHostPatterns(name string, patterns []string) (hostPatterns, error) {
	var out hostPatterns
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			return hostPatterns{}, fmt.Errorf("%s: empty pattern", name)
		}
		if prefix, ok, err := parseIPPattern(p); ok {
			if err != nil {
				return hostPatterns{}, fmt.Errorf("%s: invalid pattern %q: %w", name, p, err)
			}
			if prefix.Bits() == 0 {
				return hostPatterns{}, fmt.Errorf("%s: pattern %q matches every address", name, p)
			}
			out.prefixes = append(out.prefixes, prefix)
			continue
		}
		if strings.Contains(p, ":") {
			return hostPatterns{}, fmt.Errorf("%s: pattern %q must not carry a port", name, p)
		}
		g, err := hostglob.Compile(p)
		if err != nil {
			return hostPatterns{}, fmt.Errorf("%s: invalid pattern %q: %w", name, p, err)
		}
		if hostglob.MatchesEverySingleLabel(g) {
			return hostPatterns{}, fmt.Errorf("%s: pattern %q matches every host", name, p)
		}
		out.names = append(out.names, g)
	}
	return out, nil
}

// parseIPPattern reads p as an IP address or CIDR. ok is false when p is
// neither and should be read as a host glob; err is set when p is meant as
// one (it contains "/", or brackets) but does not parse. An IPv4-mapped
// IPv6 prefix is returned as the IPv4 prefix it covers, which is how a
// host is matched against it.
func parseIPPattern(p string) (netip.Prefix, bool, error) {
	if strings.Contains(p, "/") {
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			return netip.Prefix{}, true, err
		}
		return unmapPrefix(prefix.Masked()), true, nil
	}
	bracketed := strings.HasPrefix(p, "[") && strings.HasSuffix(p, "]")
	if bracketed {
		p = p[1 : len(p)-1]
	}
	addr, err := netip.ParseAddr(p)
	if err != nil {
		if bracketed {
			return netip.Prefix{}, true, err
		}
		return netip.Prefix{}, false, nil
	}
	if addr.Zone() != "" {
		return netip.Prefix{}, true, fmt.Errorf("must not carry a zone")
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), true, nil
}

func unmapPrefix(p netip.Prefix) netip.Prefix {
	if p.Addr().Is4In6() && p.Bits() >= 96 {
		return netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
	}
	return p
}

// match reports whether host, without its port, matches a pattern: a
// host glob by name, or an IP prefix when host is an IP literal (v4, v6
// in brackets or not, IPv4-mapped v6, or a numeric IPv4 spelling such as
// "0x0a.0x60.0.1" or "174063617" that a resolver may dial as an address).
func (h hostPatterns) match(host string) bool {
	if host == "" {
		return false
	}
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if len(h.prefixes) > 0 {
		if addr, ok := hostAddr(host); ok {
			for _, p := range h.prefixes {
				if p.Contains(addr) {
					return true
				}
			}
		}
	}
	for _, g := range h.names {
		if g.Match(host) {
			return true
		}
	}
	return false
}

// hostAddr reads host (port already removed) as an IP address.
func hostAddr(host string) (netip.Addr, bool) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.WithZone("").Unmap(), true
	}
	return parseInetAton(host)
}

// parseInetAton reads the IPv4 spellings inet_aton accepts and
// netip.ParseAddr does not: one to four parts, each decimal, octal
// (leading 0) or hex (0x), the last filling the remaining bytes.
func parseInetAton(s string) (netip.Addr, bool) {
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, part := range parts {
		base := 10
		digits := part
		switch {
		case strings.HasPrefix(part, "0x"):
			base, digits = 16, part[2:]
		case len(part) > 1 && part[0] == '0':
			base, digits = 8, part[1:]
		}
		if digits == "" && base != 8 {
			return netip.Addr{}, false
		}
		if digits == "" {
			digits = "0"
		}
		v, err := strconv.ParseUint(digits, base, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	var n uint64
	for i, v := range vals[:len(vals)-1] {
		if v > 0xff {
			return netip.Addr{}, false
		}
		n |= v << (8 * (3 - i))
	}
	last := vals[len(vals)-1]
	if last >= 1<<(8*(5-len(vals))) {
		return netip.Addr{}, false
	}
	n |= last
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
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
