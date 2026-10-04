package requestsign

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var fixtureSecret = bytes.Repeat([]byte{0x42}, 64)

func signedFixture(t *testing.T, body string, now time.Time) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "https://archive.example.test/msgvault/api/v1/cli/collections?x=a%2Bb&x=two", strings.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("X-Api-Key", "fixture-client-key")
	r.Header.Set("Content-Type", "application/json")
	require.NoError(t, Sign(r, fixtureSecret, "reader-1", now, "abcdefghijklmnopqrstuvwx01234567"))
	t.Cleanup(func() { _ = r.Body.Close() })
	return r
}

func TestSignatureIndependentCanonicalBase(t *testing.T) {
	now := time.Unix(1791000000, 0)
	r := signedFixture(t, "{}", now)
	digest := sha256.Sum256([]byte("{}"))
	params := `("@method" "@target-uri" "content-digest" "content-type" "x-api-key");created=1791000000;expires=1791000030;nonce="abcdefghijklmnopqrstuvwx01234567";alg="hmac-sha256";keyid="reader-1"`
	base := "\"@method\": POST\n\"@target-uri\": https://archive.example.test/msgvault/api/v1/cli/collections?x=a%2Bb&x=two\n\"content-digest\": sha-256=:" + base64.StdEncoding.EncodeToString(digest[:]) + ":\n\"content-type\": application/json\n\"x-api-key\": fixture-client-key\n\"@signature-params\": " + params
	mac := hmac.New(sha256.New, fixtureSecret)
	_, err := io.WriteString(mac, base)
	require.NoError(t, err)
	assert.Equal(t, "sig1="+params, r.Header.Get("Signature-Input"))
	assert.Equal(t, "sig1=:"+base64.StdEncoding.EncodeToString(mac.Sum(nil))+":", r.Header.Get("Signature"))
}

func TestSignatureCoveredComponentTampering(t *testing.T) {
	now := time.Unix(1791000000, 0)
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
	}{
		{"method", func(r *http.Request) { r.Method = http.MethodDelete }},
		{"path", func(r *http.Request) { r.URL.Path += "/other" }},
		{"query", func(r *http.Request) { r.URL.RawQuery += "&admin=true" }},
		{"auth", func(r *http.Request) { r.Header.Set("X-Api-Key", "another-key") }},
		{"body", func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader("tampered")) }},
		{"content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }},
		{"duplicate", func(r *http.Request) { r.Header.Add("X-Api-Key", "fixture-client-key") }},
		{"trailer", func(r *http.Request) { r.Trailer = http.Header{"Digest": {"anything"}} }},
		{"encoding", func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := signedFixture(t, "{}", now)
			tc.change(r)
			_, err := Verify(r, r.URL.String(), fixtureSecret, "reader-1", now)
			require.Error(t, err)
		})
	}
}

func TestSignatureTimesAndKeys(t *testing.T) {
	now := time.Unix(1791000000, 0)
	for _, tc := range []struct {
		name     string
		offset   time.Duration
		accepted bool
	}{
		{"current", 0, true}, {"five seconds future", -5 * time.Second, true},
		{"too future", -6 * time.Second, false}, {"expired", 30 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := signedFixture(t, "{}", now)
			_, err := Verify(r, r.URL.String(), fixtureSecret, "reader-1", now.Add(tc.offset))
			assert.Equal(t, tc.accepted, err == nil)
		})
	}
	r := signedFixture(t, "{}", now)
	_, err := Verify(r, r.URL.String(), bytes.Repeat([]byte{0x43}, 64), "reader-1", now)
	require.Error(t, err)
	_, err = Verify(r, r.URL.String(), fixtureSecret, "revoked", now)
	require.Error(t, err)
}

func TestCanonicalTargets(t *testing.T) {
	for _, target := range []string{"/api//v1", "/api/../admin", "/api/%2fadmin", "/api/%252fadmin", "/api/%2e/admin", "/api/\\admin"} {
		t.Run(target, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, target, nil)
			_, err := ExternalTarget("https://archive.example.test/msgvault", r)
			require.Error(t, err)
		})
	}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=a%2Bb&q=second", nil)
	r.Header.Set("Forwarded", "proto=http;host=attacker.example.test")
	r.Header.Set("X-Forwarded-Prefix", "/admin")
	target, err := ExternalTarget("https://archive.example.test/msgvault", r)
	require.NoError(t, err)
	assert.Equal(t, "https://archive.example.test/msgvault/api/v1/cli/search?q=a%2Bb&q=second", target)
}

func FuzzSignatureQueryTamper(f *testing.F) {
	f.Add("hello+world", []byte("{}"))
	f.Add("%2F&x=y", []byte{})
	f.Fuzz(func(t *testing.T, query string, body []byte) {
		if len(body) > 64<<10 {
			body = body[:64<<10]
		}
		r := signedFixture(t, string(body), time.Unix(1791000000, 0))
		// Materialize arbitrary bytes as a legal query without narrowing the input domain.
		r.URL.RawQuery += "&extra=" + base64.RawURLEncoding.EncodeToString([]byte(query))
		_, err := Verify(r, r.URL.String(), fixtureSecret, "reader-1", time.Unix(1791000000, 0))
		require.Error(t, err)
	})
}

func TestSignatureRejectsNoncanonicalParameters(t *testing.T) {
	now := time.Unix(1791000000, 0)
	for _, change := range []func(string) string{
		func(s string) string { return strings.Replace(s, "sig1=", "other=", 1) },
		func(s string) string { return s + ";created=1791000000" },
		func(s string) string { return strings.Replace(s, "expires=1791000030", "expires=1791000031", 1) },
		func(s string) string { return strings.Replace(s, `alg="hmac-sha256"`, `alg="rsa-pss-sha512"`, 1) },
		func(s string) string { return strings.Replace(s, `"content-type" `, "", 1) },
	} {
		r := signedFixture(t, "{}", now)
		r.Header.Set("Signature-Input", change(r.Header.Get("Signature-Input")))
		_, err := Verify(r, r.URL.String(), fixtureSecret, "reader-1", now)
		require.Error(t, err)
	}
}

func TestSpoolBoundsAndCancellation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("123456789"))
	_, cleanup, err := Spool(r, 8)
	if cleanup != nil {
		cleanup()
	}
	requirements.Error(err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("fixture")).WithContext(ctx)
	_, cleanup, err = Spool(r, 8)
	if cleanup != nil {
		cleanup()
	}
	requirements.ErrorIs(err, context.Canceled)
	r = httptest.NewRequest(http.MethodPost, "/", strings.NewReader("fixture"))
	digest, cleanup, err := Spool(r, 8)
	requirements.NoError(err)
	t.Cleanup(cleanup)
	want := sha256.Sum256([]byte("fixture"))
	assertions.Equal("sha-256=:"+base64.StdEncoding.EncodeToString(want[:])+":", digest)
	got, err := io.ReadAll(r.Body)
	requirements.NoError(err)
	assertions.Equal("fixture", string(got))
}

func TestSignatureCanonicalTransmittedParameterOrders(t *testing.T) {
	now := time.Unix(1791000000, 0)
	for _, parameters := range []string{
		`;created=1791000000;expires=1791000030;nonce="abcdefghijklmnopqrstuvwx01234567";alg="hmac-sha256";keyid="reader-1"`,
		`;keyid="reader-1";nonce="abcdefghijklmnopqrstuvwx01234567";alg="hmac-sha256";expires=1791000030;created=1791000000`,
	} {
		r := signedFixture(t, "{}", now)
		input := components + parameters
		r.Header.Set("Signature-Input", "sig1="+input)
		base := `"@method": POST` + "\n" + `"@target-uri": ` + r.URL.String() + "\n" + `"content-digest": ` + r.Header.Get("Content-Digest") + "\n" + `"content-type": application/json` + "\n" + `"x-api-key": fixture-client-key` + "\n" + `"@signature-params": ` + input
		mac := hmac.New(sha256.New, fixtureSecret)
		_, err := io.WriteString(mac, base)
		require.NoError(t, err)
		r.Header.Set("Signature", "sig1=:"+base64.StdEncoding.EncodeToString(mac.Sum(nil))+":")
		_, err = Verify(r, r.URL.String(), fixtureSecret, "reader-1", now)
		assert.NoError(t, err, "canonical transmitted parameter order must be retained in MAC")
	}
}

type signingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f signingRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type requestContextBody struct {
	ctx  context.Context
	data []byte
}

func (b *requestContextBody) Read(p []byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	default:
	}
	if len(b.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (*requestContextBody) Close() error { return nil }

func signedTransportFixture(t *testing.T, next http.RoundTripper) *signingTransport {
	t.Helper()
	transport, err := NewTransport(
		"https://archive.example.test/msgvault", "reader-1", fixtureSecret, 0, next,
	)
	require.NoError(t, err)
	signing, ok := transport.(*signingTransport)
	require.True(t, ok)
	return signing
}

func signedTransportRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet,
		"https://archive.example.test/msgvault/api/v1/cli/attachment?content_hash=fixture",
		nil,
	)
	require.NoError(t, err)
	r.Header.Set("X-Api-Key", "fixture-client-key")
	return r
}

func TestSigningTransportResponseBodyOutlivesHeaderTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)

		transport := signedTransportFixture(t, signingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &requestContextBody{ctx: r.Context(), data: []byte("streamed attachment")},
				Header:     make(http.Header),
				Request:    r,
			}, nil
		}))
		transport.responseHeaderTimeout = 20 * time.Millisecond
		response, err := transport.RoundTrip(signedTransportRequest(t))
		requirements.NoError(err)
		requirements.NotNil(response)
		defer func() { requirements.NoError(response.Body.Close()) }()

		time.Sleep(transport.responseHeaderTimeout * 2)
		got, err := io.ReadAll(response.Body)
		requirements.NoError(err)
		assertions.Equal("streamed attachment", string(got))
	})
}

func TestSigningTransportStillBoundsResponseHeaderWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requirements := require.New(t)

		transport := signedTransportFixture(t, signingRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}))
		transport.responseHeaderTimeout = 20 * time.Millisecond
		response, err := transport.RoundTrip(signedTransportRequest(t))
		if response != nil && response.Body != nil {
			requirements.NoError(response.Body.Close())
		}
		requirements.Nil(response)
		requirements.ErrorIs(err, context.DeadlineExceeded)
	})
}
