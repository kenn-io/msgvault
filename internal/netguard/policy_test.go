package netguard

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProhibitedHostnameRejectsPrivateAndObfuscatedNames(t *testing.T) {
	for _, hostname := range []string{
		"localhost", "api.localhost", "printer.local.", "nas.home.arpa",
		"api.internal", "nas", "images.7", "0x7f000001",
	} {
		assert.Truef(t, ProhibitedHostname(hostname), "%q", hostname)
	}
	assert.False(t, ProhibitedHostname("images.example.com"))
}

func TestProhibitedIPRejectsPrivateAndTranslatedDestinations(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fd00::1",
		"64:ff9b::a00:1", "2002:a00:1::", "2001:0:1234:5678::f5ff:fffe",
	} {
		assert.Truef(t, ProhibitedIP(netip.MustParseAddr(raw)), "%q", raw)
	}
	assert.False(t, ProhibitedIP(netip.MustParseAddr("203.0.113.7")))
}

func TestExplicitPrivateAddressAllowsOnlyTrustedPrivateRanges(t *testing.T) {
	for _, raw := range []string{"10.1.2.3", "100.80.0.8", "172.20.0.4", "192.168.1.2", "fd00::1"} {
		assert.Truef(t, ExplicitPrivateAddress(netip.MustParseAddr(raw)), "%q", raw)
	}
	for _, raw := range []string{"127.0.0.1", "169.254.169.254", "224.0.0.1", "203.0.113.7", "fc00::1%eth0"} {
		assert.Falsef(t, ExplicitPrivateAddress(netip.MustParseAddr(raw)), "%q", raw)
	}
}
