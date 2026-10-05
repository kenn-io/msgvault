package chatwoot

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientMediaReadSpansSeveralSyntheticTimeoutIntervals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		const payload = "audi"
		client, err := NewClient("https://chatwoot.example.com", 3, "synthetic-token")
		require.NoError(err)
		client.lookupMediaIP = func(_ context.Context, host string) ([]netip.Addr, error) {
			assert.Equal("media.chatwoot.example", host)
			return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
		}
		client.dialMedia = func(_ context.Context, _, address string) (net.Conn, error) {
			assert.Equal("203.0.113.7:80", address)
			clientConn, serverConn := net.Pipe()
			go func() {
				defer func() { assert.NoError(serverConn.Close()) }()
				request, readErr := http.ReadRequest(bufio.NewReader(serverConn))
				if !assert.NoError(readErr) {
					return
				}
				assert.Empty(request.Header.Get("Api_access_token"))
				assert.Empty(request.Header.Get("Authorization"))
				if _, writeErr := io.WriteString(serverConn, "HTTP/1.1 200 OK\r\nContent-Type: audio/ogg\r\nContent-Length: 4\r\nConnection: close\r\n\r\n"); writeErr != nil {
					return
				}
				for index := range len(payload) {
					if index > 0 {
						time.Sleep(100 * time.Millisecond)
					}
					if _, writeErr := io.WriteString(serverConn, payload[index:index+1]); writeErr != nil {
						return
					}
				}
			}()
			return clientConn, nil
		}
		const maxBytes = int64(16)
		var receivedLimit int64
		client.mediaTransferTimeout = func(size int64) time.Duration {
			receivedLimit = size
			return mediaTimeoutForRate(size, 1, 75*time.Millisecond)
		}

		body, _, _, err := client.OpenMedia(t.Context(), "http://media.chatwoot.example/slow-audio", maxBytes)
		require.NoError(err)
		got, err := io.ReadAll(body)
		require.NoError(err)
		require.NoError(body.Close())
		assert.Equal(payload, string(got))
		assert.Equal(maxBytes, receivedLimit, "the per-attachment cap must set the body deadline")
	})
}
