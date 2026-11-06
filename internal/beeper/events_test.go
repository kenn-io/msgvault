package beeper

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestEventURLSameOrigin(t *testing.T) {
	for _, tt := range []struct{ base, endpoint, want string }{
		{"http://example.com", "/v1/ws", "ws://example.com/v1/ws"},
		{"https://example.com", "wss://EXAMPLE.com:443/v1/ws", "wss://EXAMPLE.com:443/v1/ws"},
		{"http://example.com:80", "ws://example.com/events?q=1", "ws://example.com/events?q=1"},
		{"https://example.com", "ws://example.com/ws", ""},
		{"http://example.com", "ws://other.example/ws", ""},
		{"http://example.com", "//other.example/ws", ""},
		{"http://example.com", "ws://u:p@example.com/ws", ""},
		{"http://example.com", "/ws#fragment", ""},
		{"http://example.com", "file:///ws", ""},
		{"http://example.com", "http://example.com/ws", ""},
		{"http://example.com", "", ""},
	} {
		t.Run(tt.base+tt.endpoint, func(t *testing.T) {
			u, err := resolveEventURL(tt.base, tt.endpoint)
			if tt.want == "" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, u.String())
		})
	}
}

func TestEventDiscoveryRejectsRedirect(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer synthetic", r.Header.Get("Authorization"))
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer server.Close()
	c := NewClient(server.URL, func(context.Context) (string, error) { return "synthetic", nil }, 100)
	_, err := c.discoverEvents(context.Background(), "synthetic")
	require.Error(t, err)
	assert.False(t, leaked.Load())
}

func TestEventDiscoveryBoundsAndAuthorization(t *testing.T) {
	for _, status := range []int{200, 401, 403, 404} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(strings.Repeat("x", (64<<10)+1)))
			}))
			defer server.Close()
			c := NewClient(server.URL, nil, 100)
			_, err := c.discoverEvents(context.Background(), "synthetic")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "xxxxxxxx")
			if status == 401 || status == 403 {
				assert.ErrorIs(t, err, ErrUnauthorized)
			}
		})
	}
}

func FuzzEventURLOrigin(f *testing.F) {
	for _, s := range []string{"/v1/ws", "wss://example.com:443/events", "//evil.example/ws", "wss://u:p@example.com/ws", "/ws#f", "", "../events"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := resolveEventURL("https://example.com", s)
		if err != nil {
			return
		}
		assert.Equal(t, "wss", u.Scheme)
		assert.True(t, strings.EqualFold("example.com", u.Hostname()))
		assert.Contains(t, []string{"", "443"}, u.Port())
		assert.Nil(t, u.User)
		assert.Empty(t, u.Fragment)
	})
}

func FuzzEventRelativeURL(f *testing.F) {
	for _, s := range []string{"", "sample", "../@?#", "東京"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		endpoint := "/v1/ws?hint=" + url.QueryEscape(s)
		u, err := resolveEventURL("https://example.com", endpoint)
		require.NoError(t, err)
		assert.Equal(t, "wss://example.com"+endpoint, u.String())
		assert.Equal(t, s, u.Query().Get("hint"))
	})
}

func TestHegelEventEnvelopeAcceptsIDOnlyHints(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.SampledFrom([]string{"chat.upserted", "chat.deleted", "message.upserted", "message.deleted"}))
		chat := hegel.Draw(ht, hegel.Text().MinSize(1))
		ids := hegel.Draw(ht, hegel.Lists(hegel.Text().MinSize(1)).MinSize(1))
		seq := hegel.Draw(ht, hegel.Integers[uint64](0, math.MaxUint64))
		b, err := json.Marshal(map[string]any{"type": kind, "chatID": chat, "ids": ids, "seq": seq})
		require.NoError(ht, err)
		e, err := decodeEventEnvelope(b)
		require.NoError(ht, err)
		assert.True(ht, e.isHint())
		assert.Equal(ht, kind, e.Type)
		assert.Equal(ht, chat, e.ChatID)
		assert.Equal(ht, ids, e.IDs)
		require.NotNil(ht, e.Seq)
		assert.Equal(ht, seq, *e.Seq)
	})
}
func TestHegelEventEnvelopeRejectsMissingRequiredField(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		kind := hegel.Draw(ht, hegel.SampledFrom([]string{"chat.upserted", "chat.deleted", "message.upserted", "message.deleted"}))
		field := hegel.Draw(ht, hegel.SampledFrom([]string{"chatID", "ids", "seq"}))
		value := map[string]any{"type": kind, "chatID": "chat-example", "ids": []string{"message-example"}, "seq": uint64(1)}
		delete(value, field)
		b, err := json.Marshal(value)
		require.NoError(ht, err)
		_, err = decodeEventEnvelope(b)
		require.Error(ht, err)
	})
}

func TestEventDiscoveryExactSizeBoundary(t *testing.T) {
	for _, size := range []int{(64 << 10) - 1, 64 << 10, (64 << 10) + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			prefix := `{"endpoints":{"ws_events":"/events"},"padding":"`
			suffix := `"}`
			body := prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			c := NewClient(server.URL, testToken, 100)
			endpoint, err := c.discoverEvents(t.Context(), "synthetic")
			if size > 64<<10 {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "ws"+strings.TrimPrefix(server.URL, "http")+"/events", endpoint.String())
		})
	}
}
func TestHegelEventURLOriginSpellings(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		secure := hegel.Draw(ht, hegel.Booleans())
		host := hegel.Draw(ht, hegel.SampledFrom([]string{"example.com", "EXAMPLE.com", "127.0.0.1", "[::1]"}))
		explicitDefault := hegel.Draw(ht, hegel.Booleans())
		customPort := hegel.Draw(ht, hegel.Booleans())
		port := hegel.Draw(ht, hegel.Integers(1024, 65535))
		query := hegel.Draw(ht, hegel.Text())
		httpScheme, wsScheme, defaultPort := "http", "ws", "80"
		if secure {
			httpScheme, wsScheme, defaultPort = "https", "wss", "443"
		}
		suffix := ""
		endpointPort := ":" + defaultPort
		if explicitDefault {
			suffix = ":" + defaultPort
			endpointPort = ""
		}
		if customPort {
			suffix = ":" + strconv.Itoa(port)
			endpointPort = suffix
		}
		base := httpScheme + "://" + host + suffix
		discovered := wsScheme + "://" + strings.ToLower(host) + endpointPort + "/events?hint=" + url.QueryEscape(query)
		endpoint, err := resolveEventURL(base, discovered)
		require.NoError(ht, err)
		assert.Equal(ht, discovered, endpoint.String())
		_, err = resolveEventURL(base, wsScheme+"://other.example"+endpointPort+"/events")
		require.Error(ht, err)
	})
}
func TestEventInvalidOriginDoesNotSendCredentials(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	for _, base := range []string{"http://user:pass@" + strings.TrimPrefix(server.URL, "http://"), server.URL + "#fragment", "ftp://" + strings.TrimPrefix(server.URL, "http://")} {
		c := NewClient(base, testToken, 100)
		_, err := c.discoverEvents(t.Context(), "synthetic")
		require.Error(t, err)
	}
	assert.Zero(t, calls.Load())
}

func TestHegelEventRetryJitterBounded(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		delay := hegel.Draw(ht, hegel.Integers[time.Duration](1, time.Duration(math.MaxInt64)))
		entropy := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint8](0, 255)))
		got := jitterEventRetry(delay, bytes.NewReader(entropy))
		assert.GreaterOrEqual(ht, got, delay/2)
		assert.Less(ht, got, delay)
	})
}
