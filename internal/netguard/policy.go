// Package netguard identifies destinations that network clients must not
// contact. It centralizes the SSRF policy shared by user-controlled remote
// fetchers and protocol clients.
package netguard

import (
	"net/netip"
	"strings"
)

var prohibitedV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

var prohibitedV6 = []netip.Prefix{
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

var (
	v4CompatPrefix = netip.MustParsePrefix("::/96")
	nat64Prefix    = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour      = netip.MustParsePrefix("2002::/16")
	teredoPrefix   = netip.MustParsePrefix("2001::/32")
)

// ProhibitedIP reports whether addr is invalid, scoped, private, reserved, or
// an IPv6 transition address that ultimately reaches a prohibited IPv4
// destination.
func ProhibitedIP(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Zone() != "" {
		return true
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if embedded, ok := embeddedIPv4(addr); ok {
			addr = embedded
		}
	}
	prefixes := prohibitedV4
	if addr.Is6() {
		prefixes = prohibitedV6
	}
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	raw := addr.As16()
	switch {
	case v4CompatPrefix.Contains(addr) || nat64Prefix.Contains(addr):
		return netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}), true
	case sixToFour.Contains(addr):
		return netip.AddrFrom4([4]byte{raw[2], raw[3], raw[4], raw[5]}), true
	case teredoPrefix.Contains(addr):
		return netip.AddrFrom4([4]byte{raw[12] ^ 0xff, raw[13] ^ 0xff, raw[14] ^ 0xff, raw[15] ^ 0xff}), true
	default:
		return netip.Addr{}, false
	}
}

// ProhibitedHostname reports whether hostname is a reserved/private name,
// unqualified name, or an obfuscated numeric destination.
func ProhibitedHostname(hostname string) bool {
	host := strings.ToLower(strings.TrimSpace(hostname))
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return true
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 || decimalLabel(labels[len(labels)-1]) || hexLabel(labels[len(labels)-1]) {
		return true
	}
	for _, suffix := range []string{"localhost", "local", "internal", "home.arpa"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func decimalLabel(label string) bool {
	if label == "" {
		return false
	}
	for _, r := range label {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hexLabel(label string) bool {
	rest, ok := strings.CutPrefix(label, "0x")
	if !ok {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
