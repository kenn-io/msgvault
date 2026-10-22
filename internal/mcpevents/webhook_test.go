package mcpevents

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
)

func tlsWebhook(t *testing.T, handler http.HandlerFunc) *webhookClient {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Require.NoError(t, err)
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "receiver.example.net"}, DNSNames: []string{"receiver.example.net", "other.example.net"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	Require.NoError(t, err)
	receiver := httptest.NewUnstartedServer(handler)
	receiver.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	receiver.StartTLS()
	t.Cleanup(receiver.Close)
	w, err := newWebhookClient(nil)
	Require.NoError(t, err)
	w.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
	}
	w.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
	}
	transport, ok := receiver.Client().Transport.(*http.Transport)
	Require.True(t, ok)
	w.transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	t.Cleanup(w.transport.CloseIdleConnections)
	return w
}

func TestWebhookChallengeOverTLSWithStandardSignature(t *testing.T) {
	secret := make([]byte, 32)
	w := tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if !Assert.NoError(t, err) {
			return
		}
		Assert.Equal(t, "sub_synthetic", r.Header.Get("X-Mcp-Subscription-Id"))
		Assert.Equal(t, webhookSignature(secret, r.Header.Get("Webhook-Id"), r.Header.Get("Webhook-Timestamp"), body), r.Header.Get("Webhook-Signature"))
		var verification struct {
			Type      string `json:"type"`
			Challenge string `json:"challenge"`
		}
		if !Assert.NoError(t, json.Unmarshal(body, &verification)) {
			return
		}
		Assert.Equal(t, "verification", verification.Type)
		challenge, err := base64.RawURLEncoding.DecodeString(verification.Challenge)
		if !Assert.NoError(t, err) {
			return
		}
		Assert.Len(t, challenge, 32)
		if !Assert.NoError(t, json.NewEncoder(rw).Encode(map[string]string{"challenge": verification.Challenge})) {
			return
		}
	})
	Require.NoError(t, w.verify(t.Context(), "https://receiver.example.net/events", "sub_synthetic", secret, nil))
}

func TestWebhookChallengeValidatesResponseStatusAndSize(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		reason        string
		echoChallenge bool
		responseSize  int
		wantError     bool
	}{
		{name: "redirect", status: 302, reason: "challenge_failed", wantError: true},
		{name: "mismatch", status: 200, body: `{"challenge":"wrong"}`, reason: "challenge_failed", wantError: true},
		{name: "matching challenge at limit", status: 200, echoChallenge: true, responseSize: 4096},
		{name: "matching challenge over limit", status: 200, echoChallenge: true, responseSize: 4097, reason: "challenge_failed", wantError: true},
		{name: "client", status: 403, reason: "http_4xx", wantError: true},
		{name: "server", status: 503, reason: "http_5xx", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			w := tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
				rw.Header().Set("Location", "https://other.example.net/secret-marker")
				body := []byte(tc.body)
				if tc.echoChallenge {
					var verification struct {
						Challenge string `json:"challenge"`
					}
					if !assert.NoError(json.NewDecoder(r.Body).Decode(&verification)) {
						return
					}
					var err error
					body, err = json.Marshal(map[string]string{"challenge": verification.Challenge})
					if !assert.NoError(err) {
						return
					}
					if tc.responseSize > len(body) {
						body = append(body, strings.Repeat(" ", tc.responseSize-len(body))...)
					}
				}
				rw.WriteHeader(tc.status)
				_, err := rw.Write(body)
				if !assert.NoError(err) {
					return
				}
			})
			err := w.verify(t.Context(), "https://receiver.example.net/callback-marker", "sub_synthetic", make([]byte, 32), nil)
			if !tc.wantError {
				require.NoError(err)
				return
			}
			require.Error(err)
			var eventErr *Error
			require.ErrorAs(err, &eventErr)
			assert.Equal(tc.reason, eventErr.Reason)
			assert.NotContains(err.Error(), "callback-marker")
		})
	}
}

func TestCallbackResolutionRequiresEveryPublicAddressAndFullOriginPins(t *testing.T) {
	require := Require.New(t)
	w, err := newWebhookClient([]TrustedCallback{{Origin: "https://receiver.example.net:8443", Addresses: []string{"10.1.2.3"}}})
	require.NoError(err)
	w.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("203.0.113.7"), netip.MustParseAddr("10.1.2.3")}, nil
	}
	_, err = w.addresses(t.Context(), "receiver.example.net", "443")
	require.Error(err)
	addresses, err := w.addresses(t.Context(), "receiver.example.net", "8443")
	require.NoError(err)
	Assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.1.2.3")}, addresses)
	for _, address := range []string{"127.0.0.1", "169.254.169.254", "::1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:7f00:1::", "2001:0::ffff:80ff:fffe", "fc00::1", "fe80::1"} {
		w.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr(address)}, nil
		}
		_, err := w.addresses(t.Context(), "receiver.example.net", "443")
		require.Error(err, address)
	}
}

func TestCallbackURLPolicy(t *testing.T) {
	for _, raw := range []string{"http://receiver.example.net", "https://receiver.example.net:9443", "https://user:pass@receiver.example.net", "https://receiver.example.net/#fragment", "https://localhost", "https://127.0.0.1", "https://[::1]", "https://receiver.example.net:0443", "https://receiver.example.net/%zz"} {
		_, err := callbackURL(raw)
		Require.Error(t, err)
		Assert.NotContains(t, err.Error(), raw)
	}
	for _, raw := range []string{"https://receiver.example.net/events?opaque=1", "https://receiver.example.net:443/events", "https://receiver.example.net:8443/events", "https://[2606:4700::1111]/events"} {
		_, err := callbackURL(raw)
		Require.NoError(t, err)
	}
}

func TestWebhookDeliveryReturnsHeadersWithoutWaitingForBody(t *testing.T) {
	for _, responseStatus := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(responseStatus), func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			current, previous := []byte(strings.Repeat("n", 32)), []byte(strings.Repeat("p", 32))
			body := []byte(`{"eventId":"evt_synthetic"}`)
			// The receiver flushes headers and a short body, then keeps the response
			// open. Delivery must finish without waiting for EOF or a body-size limit.
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			w := tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) {
				Assert.Equal(t, webhookSignature(current, "evt_synthetic", r.Header.Get("Webhook-Timestamp"), body)+" "+webhookSignature(previous, "evt_synthetic", r.Header.Get("Webhook-Timestamp"), body), r.Header.Get("Webhook-Signature"))
				rw.Header().Set("Retry-After", "120")
				rw.WriteHeader(responseStatus)
				_, err := io.WriteString(rw, "ok")
				if !Assert.NoError(t, err) {
					return
				}
				flusher, ok := rw.(http.Flusher)
				if !Assert.True(t, ok, "the test receiver must stream its response") {
					return
				}
				flusher.Flush()
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			type result struct {
				status int
				header string
				err    error
			}
			done := make(chan result, 1)
			go func() {
				status, header, err := w.post(ctx, "https://receiver.example.net/events", "sub_synthetic", "evt_synthetic", body, current, previous, nil)
				done <- result{status, header, err}
			}()
			var got result
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				require.FailNow("post must return the headers while the receiver holds its body open")
			}
			releaseOnce.Do(func() { close(release) })
			require.NoError(got.err)
			assert.Equal(responseStatus, got.status)
			assert.Equal("120", got.header)
			status, _, err := w.post(ctx, "https://receiver.example.net/events", "sub_synthetic", "evt_synthetic", make([]byte, 262145), current, nil, nil)
			require.Error(err)
			assert.Zero(status)
		})
	}
}

// stalledLookupWebhook returns a client whose callback lookup waits until
// release is called, and a channel that receives each dial's result.
func stalledLookupWebhook(t *testing.T) (w *webhookClient, lookupStarted chan struct{}, release func(), dials chan error) {
	t.Helper()
	w = tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(http.StatusNoContent) })
	lookupStarted, resume, dials := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	release = func() { once.Do(func() { close(resume) }) }
	t.Cleanup(release)
	w.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		close(lookupStarted)
		<-resume
		return []netip.Addr{netip.MustParseAddr("203.0.113.7")}, nil
	}
	dial := w.transport.DialContext
	w.transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		dials <- err
		return conn, err
	}
	return w, lookupStarted, release, dials
}

func postInBackground(ctx context.Context, w *webhookClient, guard func(context.Context) error) chan error {
	done := make(chan error, 1)
	go func() {
		_, _, err := w.post(ctx, "https://receiver.example.net/events", "sub_synthetic", "msg_synthetic", []byte(`{}`), make([]byte, 32), nil, guard)
		done <- err
	}()
	return done
}

func TestDialGuardDoesNotRunAfterRequestEnds(t *testing.T) {
	require := Require.New(t)
	w, lookupStarted, release, dials := stalledLookupWebhook(t)
	var requestEnded atomic.Bool
	var lateChecks atomic.Int32
	guard := func(ctx context.Context) error {
		if requestEnded.Load() && ctx.Err() == nil {
			lateChecks.Add(1)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := postInBackground(ctx, w, guard)
	select {
	case <-lookupStarted:
	case <-time.After(10 * time.Second):
		require.FailNow("callback lookup did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(err)
	case <-time.After(10 * time.Second):
		require.FailNow("cancelled request did not return")
	}
	requestEnded.Store(true)
	release()
	select {
	case err := <-dials:
		require.Error(err, "a dial that outlives its request must not connect")
	case <-time.After(10 * time.Second):
		require.FailNow("orphaned dial did not finish")
	}
	Assert.Zero(t, lateChecks.Load(), "the dial guard must not run Store checks for a finished request")
}

func TestDialGuardWaitEndsWithRequest(t *testing.T) {
	require := Require.New(t)
	w := tlsWebhook(t, func(rw http.ResponseWriter, r *http.Request) { rw.WriteHeader(http.StatusNoContent) })
	var resolved atomic.Bool
	resolve := w.resolve
	w.resolve = func(ctx context.Context, network, host string) ([]netip.Addr, error) {
		resolved.Store(true)
		return resolve(ctx, network, host)
	}
	guardStarted, guardReturned := make(chan struct{}), make(chan error, 1)
	guard := func(ctx context.Context) error {
		if !resolved.Load() {
			return nil
		}
		close(guardStarted)
		// Stands in for a wait on the archive gate.
		<-ctx.Done()
		guardReturned <- ctx.Err()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := postInBackground(ctx, w, guard)
	select {
	case <-guardStarted:
	case <-time.After(10 * time.Second):
		require.FailNow("dial guard did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.Error(err)
	case <-time.After(10 * time.Second):
		require.FailNow("cancelled request did not return")
	}
	select {
	case err := <-guardReturned:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(10 * time.Second):
		require.FailNow("dial guard kept waiting after its request ended")
	}
}
