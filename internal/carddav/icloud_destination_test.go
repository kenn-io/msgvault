package carddav

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICloudPartitionMustResolveOnlyToPublicAddresses(t *testing.T) {
	for _, addresses := range [][]netip.Addr{
		{netip.MustParseAddr("10.1.2.3")}, {netip.MustParseAddr("127.0.0.1")},
		{netip.MustParseAddr("169.254.1.2")}, {netip.MustParseAddr("::ffff:10.1.2.3")},
		{netip.MustParseAddr("203.0.113.9"), netip.MustParseAddr("10.1.2.3")},
	} {
		var seen []string
		client := newICloudTLSFixture(t, iCloudFixtureHandler(t, "https://p42-contacts.icloud.com/synthetic/home/", "", &seen))
		listener, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
		require.NoError(t, err)
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			buffer := make([]byte, 512)
			for {
				n, remote, readErr := listener.ReadFrom(buffer)
				if readErr != nil {
					return
				}
				answers := []netip.Addr{netip.MustParseAddr("203.0.113.9")}
				if bytes.Contains(buffer[:n], []byte("p42-contacts")) {
					answers = addresses
				}
				_, _ = listener.WriteTo(fixtureDNSResponse(buffer[:n], answers...), remote)
			}
		}()
		client.resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, listener.LocalAddr().String())
		}}
		// A private exception on the entered origin is not inherited during
		// bootstrap transfer. All authenticated sockets remain public-pinned.
		client.trustedOrigin = client.origin
		client.trustedAddresses = []netip.Addr{netip.MustParseAddr("10.9.8.7")}
		_, err = NewICloudService(nil, client).DiscoverConnection(t.Context(), ICloudDiscoveryURL)
		require.ErrorIs(t, err, ErrUnsafeTarget)
		assert.Len(t, seen, 2)
	}
}
