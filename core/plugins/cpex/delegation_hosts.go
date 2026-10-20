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
// credentials may be sent to: `.`-delimited globs, as in skip_hosts and
// authproxy-routes, matched against the host without its port, and IP
// addresses or CIDRs, matched against a host that is a canonical IP
// literal (see hostSet.matchExact).
type credentialHosts map[string]hostSet

// hostPatterns is the compiled strict_hosts list (see hostSet.matchAny).
type hostPatterns = hostSet

// hostSet is a compiled list of host patterns: `.`-delimited name globs,
// and IP prefixes. A host that reads as an address is matched against the
// prefixes only, never the globs, and a name against the globs only, so a
// glob cannot be satisfied by a numeric spelling the dialer reads as an
// address, and a name that resolves to a listed address is not taken for
// it.
type hostSet struct {
	names    []*glob.Pattern
	prefixes []netip.Prefix
}

// newHostPatterns compiles the strict_hosts patterns.
func newHostPatterns(name string, patterns []string) (hostPatterns, error) {
	return compileHostSet(name, patterns)
}

// compileHostSet compiles the patterns of the config key key. Each is a
// host glob, an IP address ("10.96.0.10", "fd00::a", "[fd00::a]") or a
// CIDR ("10.96.0.0/12", "fd00:10:96::/112"). A pattern that is empty,
// carries a port or a zone, matches every host or every address, or is a
// glob shaped like an address ("10.96.*.*", "*.1": use a CIDR) is refused.
func compileHostSet(key string, patterns []string) (hostSet, error) {
	var out hostSet
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			return hostSet{}, fmt.Errorf("%s: empty pattern", key)
		}
		if prefix, ok, err := parseIPPattern(p); ok {
			if err != nil {
				return hostSet{}, fmt.Errorf("%s: invalid pattern %q: %w", key, p, err)
			}
			if prefix.Bits() == 0 {
				return hostSet{}, fmt.Errorf("%s: pattern %q matches every address", key, p)
			}
			out.prefixes = append(out.prefixes, prefix)
			continue
		}
		if strings.Contains(p, ":") {
			return hostSet{}, fmt.Errorf("%s: pattern %q must not carry a port", key, p)
		}
		if addressShapedGlob(p) {
			return hostSet{}, fmt.Errorf("%s: pattern %q is shaped like an address; list an IP address or CIDR instead", key, p)
		}
		g, err := hostglob.Compile(p)
		if err != nil {
			return hostSet{}, fmt.Errorf("%s: invalid pattern %q: %w", key, p, err)
		}
		if hostglob.MatchesEverySingleLabel(g) {
			return hostSet{}, fmt.Errorf("%s: pattern %q matches every host", key, p)
		}
		out.names = append(out.names, g)
	}
	return out, nil
}

// addressShapedGlob reports whether a name glob is made of numeric and
// wildcard labels only, with at least one numeric label: the shape of an
// IPv4 address, which name globs never match (see hostSet).
func addressShapedGlob(p string) bool {
	numeric := false
	for _, label := range strings.Split(p, ".") {
		switch {
		case numericLabel(label):
			numeric = true
		case strings.Trim(label, "*?") == "":
		default:
			return false
		}
	}
	return numeric
}

// numericLabel reports whether label is an inet_aton number part:
// decimal or octal digits, or 0x and hex digits.
func numericLabel(label string) bool {
	digits := label
	hex := strings.HasPrefix(label, "0x")
	if hex {
		digits = label[2:]
	}
	if digits == "" {
		return hex
	}
	for _, c := range digits {
		if !(c >= '0' && c <= '9' || hex && c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// parseIPPattern reads p as an IP address or CIDR. ok is false when p is
// neither and should be read as a host glob; err is set when p is meant as
// one (it contains "/" or brackets) but does not parse. An IPv4-mapped
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

// hostKind is how a request's host reads to the dialer.
type hostKind int

const (
	hostName      hostKind = iota // a name, resolved by DNS
	hostCanonical                 // an IP literal netip parses
	hostAton                      // a numeric spelling inet_aton reads as an IPv4 address
)

// classifyHost removes the port, brackets and a zone ("%eth0", or "%25eth0"
// as a URL spells it) from host and reads what remains. A trailing dot is
// kept: "10.96.0.10." does not parse as an address, so the dialer may look
// it up as a name, and it is classified as an inet_aton spelling instead.
func classifyHost(host string) (hostKind, netip.Addr, string) {
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	literal, zoned := host, false
	if i := strings.IndexByte(literal, '%'); i >= 0 {
		literal, zoned = literal[:i], true
	}
	if addr, err := netip.ParseAddr(literal); err == nil {
		if !zoned || addr.Is6() {
			return hostCanonical, addr.Unmap(), host
		}
		return hostAton, addr, host
	}
	if addr, ok := parseInetAton(strings.TrimSuffix(literal, ".")); ok {
		return hostAton, addr, host
	}
	return hostName, netip.Addr{}, strings.TrimSuffix(host, ".")
}

// matchAny reports whether host may reach a listed destination: a name
// matching a glob, or any spelling of an address the dialer may connect
// to (an IP literal in any case, with leading zeros, IPv4-mapped, with a
// zone or a trailing dot, or a numeric inet_aton form such as
// "0x0a.0x60.0.1", "010.096.0.1", "10.6291457" or "174063617") inside a
// listed prefix. strict_hosts uses it: it widens a check, so every
// spelling that may arrive there must be caught. A name that DNS resolves
// to a listed address is not caught; list it by name.
func (h hostSet) matchAny(host string) bool {
	if host == "" {
		return false
	}
	kind, addr, name := classifyHost(host)
	if kind == hostName {
		return h.matchName(name)
	}
	return h.matchAddr(addr)
}

// matchExact reports whether host is certainly a listed destination: a
// name matching a glob, or a canonical IP literal inside a listed prefix.
// An inet_aton spelling matches nothing, since what it reaches depends on
// the resolver. delegated_credential_hosts uses it: it decides where a
// credential may go, so a spelling it cannot be sure of withholds it.
func (h hostSet) matchExact(host string) bool {
	if host == "" {
		return false
	}
	switch kind, addr, name := classifyHost(host); kind {
	case hostName:
		return h.matchName(name)
	case hostCanonical:
		return h.matchAddr(addr)
	}
	return false
}

// match is matchAny, the strict_hosts rule.
func (h hostSet) match(host string) bool { return h.matchAny(host) }

func (h hostSet) matchName(name string) bool {
	for _, g := range h.names {
		if g.Match(name) {
			return true
		}
	}
	return false
}

func (h hostSet) matchAddr(addr netip.Addr) bool {
	for _, p := range h.prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// parseInetAton reads the IPv4 spellings inet_aton accepts: one to four
// parts, each decimal, octal (leading 0) or hex (0x), the last filling
// the remaining bytes.
func parseInetAton(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, part := range parts {
		if !numericLabel(part) {
			return netip.Addr{}, false
		}
		base, digits := 10, part
		switch {
		case strings.HasPrefix(part, "0x"):
			base, digits = 16, part[2:]
		case len(part) > 1 && part[0] == '0':
			base, digits = 8, part[1:]
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
// needs at least one pattern; patterns are refused as compileHostSet
// refuses them.
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
		set, err := compileHostSet(fmt.Sprintf("delegated_credential_hosts: target %q", target), patterns)
		if err != nil {
			return nil, err
		}
		out[target] = set
	}
	return out, nil
}

// allows reports whether a credential for target may be sent to host
// (see hostSet.matchExact).
func (h credentialHosts) allows(target, host string) bool {
	if target == "" || host == "" {
		return false
	}
	set, ok := h[target]
	return ok && set.matchExact(host)
}

// credentialTarget is the key a delegated credential is bound by: the
// target its delegate() step named, else its audience.
func credentialTarget(target, audience string) string {
	if target != "" {
		return target
	}
	return audience
}
