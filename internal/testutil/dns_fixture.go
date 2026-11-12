package testutil

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

// FixtureResolver answers DNS queries with the supplied test address.
func FixtureResolver(t *testing.T, address netip.Addr) *net.Resolver {
	t.Helper()
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
			_, _ = listener.WriteTo(fixtureCardDAVDNSResponse(buffer[:n], address), remote)
		}
	}()
	dialer := net.Dialer{}
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, listener.LocalAddr().String())
	}}
}

func fixtureCardDAVDNSResponse(request []byte, address netip.Addr) []byte {
	questionEnd := 12
	for questionEnd < len(request) && request[questionEnd] != 0 {
		questionEnd += int(request[questionEnd]) + 1
	}
	questionEnd += 5
	response := append([]byte{}, request[:2]...)
	response = append(response, 0x81, 0x80, 0x00, 0x01)
	// #nosec G602 -- questionEnd >= 4 proves both question-type indexes are in bounds.
	if address.Is4() && questionEnd >= 4 && request[questionEnd-4] == 0 && request[questionEnd-3] == 1 {
		response = append(response, 0x00, 0x01)
	} else {
		response = append(response, 0, 0)
	}
	response = append(response, 0, 0, 0, 0)
	response = append(response, request[12:questionEnd]...)
	// #nosec G602 -- questionEnd >= 4 proves both question-type indexes are in bounds.
	if address.Is4() && questionEnd >= 4 && request[questionEnd-4] == 0 && request[questionEnd-3] == 1 {
		response = append(response, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0x3c, 0, 4)
		response = append(response, address.AsSlice()...)
	}
	return response
}
