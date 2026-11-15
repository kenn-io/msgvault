package imap

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatchInboxConnectivityFailureClass(t *testing.T) {
	for _, cause := range testConnectivityErrors {
		t.Run(cause.Error(), func(t *testing.T) {
			err := fmt.Errorf("dial IMAP: %w", &net.OpError{Op: "dial", Net: "tcp", Err: cause})
			assert.True(t, isRetryableTransportError(err))
			assert.ErrorIs(t, idleResult(t.Context(), err, true), ErrIdleDisconnected)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			assert.ErrorIs(t, idleResult(ctx, err, true), context.Canceled)
		})
	}
	for _, cause := range []error{
		&imap.Error{Type: imap.StatusResponseTypeNo, Code: imap.ResponseCodeAuthenticationFailed},
		errors.New("missing token source"),
		&net.DNSError{Err: "no such host", IsNotFound: true},
		&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
		tls.RecordHeaderError{},
		tls.AlertError(40),
	} {
		t.Run(cause.Error(), func(t *testing.T) {
			err := fmt.Errorf("connect IMAP: %w", cause)
			assert.False(t, isRetryableTransportError(err))
			assert.ErrorIs(t, idleResult(t.Context(), err, true), ErrIdleAuthentication)
		})
	}
}

func TestWatchInboxConnectionRefused(t *testing.T) {
	for _, mode := range []string{"recover", "exhaust", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			address := listener.Addr().String()
			require.NoError(t, listener.Close())
			client := (&idleWire{address: address}).client(t)
			client.connectRetryInitialInterval = time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var delays []time.Duration
			client.connectRetryNotify = func(_ error, delay time.Duration) {
				delays = append(delays, delay)
				switch mode {
				case "recover":
					// The same local endpoint comes back only after an actual
					// refused dial has entered the production backoff path.
					listener, err := net.Listen("tcp", address)
					require.NoError(t, err)
					startIdleWireOnListener(t, listener, "IDLE", "")
				case "cancel":
					cancel()
				}
			}
			woke := false
			err = client.WatchInbox(ctx, func() { woke = true; cancel() })
			wantErr, wantRetries := error(context.Canceled), 1
			if mode == "exhaust" {
				wantErr, wantRetries = ErrIdleDisconnected, 3
			}
			assert.ErrorIs(t, err, wantErr)
			assert.Equal(t, mode == "recover", woke)
			require.Len(t, delays, wantRetries)
			base := time.Millisecond
			for _, delay := range delays {
				assert.GreaterOrEqual(t, delay, base/2)
				assert.LessOrEqual(t, delay, base+base/2)
				base *= 3
			}
			require.NoError(t, client.Close())
		})
	}
}
