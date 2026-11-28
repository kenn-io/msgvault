package netguard

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
)

type fixedResolver []netip.Addr

func (r fixedResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r, nil
}
func TestPinnedAddressesRejectsMixedAnswersAndHonorsPins(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	target, err := url.Parse("https://example.com/hook")
	require.NoError(err)
	_, err = PinnedAddresses(t.Context(), fixedResolver{netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("127.0.0.1")}, target, nil, nil, false)
	require.Error(err)
	origin, err := url.Parse("https://example.com")
	require.NoError(err)
	addresses, err := PinnedAddresses(t.Context(), fixedResolver{netip.MustParseAddr("127.0.0.1")}, target, origin, []netip.Addr{netip.MustParseAddr("10.0.0.5")}, false)
	require.NoError(err)
	assert.Equal([]netip.AddrPort{netip.MustParseAddrPort("10.0.0.5:443")}, addresses)
	var dialed string
	dial := PinnedDialer(addresses, func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, context.Canceled
	})
	_, err = dial(t.Context(), "tcp", "example.com:443")
	require.Error(err)
	assert.Equal("10.0.0.5:443", dialed)
}
