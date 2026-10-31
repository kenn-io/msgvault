package x

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimelineResponseLossDoesNotReplayGET(t *testing.T) {
	for _, mode := range []string{"default", "provided", "credentials"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "credentials" {
					assert.Equal(t, "Bearer access-secret", r.Header.Get("Authorization"))
				}
				if calls.Add(1) == 2 {
					connection, _, err := http.NewResponseController(w).Hijack()
					require.NoError(t, err)
					require.NoError(t, connection.Close())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":[`+rootJSON+`]}`)
			}))
			defer server.Close()
			var base *http.Client
			if mode != "default" {
				base = server.Client()
			}
			if mode == "credentials" {
				m := NewTokenManager(tokenTempDir(t))
				record := testRecord()
				record.Expiry = time.Now().Add(time.Hour)
				require.NoError(t, m.Save(t.Context(), record))
				credentials := m.Credentials(record.OwnerID, base)
				_, err := credentials.Prepare(t.Context())
				require.NoError(t, err)
				base = credentials.HTTPClient()
			}
			client := NewClient(server.URL, base)
			request := TimelineRequest{OwnerID: "1001", MaxResults: 5}
			_, err := client.Timeline(t.Context(), request)
			require.NoError(t, err)
			_, err = client.Timeline(t.Context(), request)
			assert.Equal(t, int32(2), calls.Load(), "response loss must return to admission before another GET")
			require.ErrorIs(t, err, ErrTransport)
		})
	}
}

func TestRequestTransportPreservesTLSProxyAndBoundsRequests(t *testing.T) {
	var proxyCalls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, 1, r.ProtoMajor)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"id":"1001","name":"Example Author","protected":false,"username":"example_author"}}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	original := server.Client().Transport.(*http.Transport).Clone()
	original.ForceAttemptHTTP2 = true
	original.Protocols = new(http.Protocols)
	original.Protocols.SetHTTP1(true)
	original.Protocols.SetHTTP2(true)
	original.TLSClientConfig.MinVersion = tls.VersionTLS12
	original.TLSClientConfig.NextProtos = []string{"h2", "http/1.1"}
	original.Proxy = func(*http.Request) (*url.URL, error) { proxyCalls.Add(1); return nil, nil }
	supplied := &http.Client{Transport: original}
	client := NewClient(server.URL, supplied)
	_, err := client.Me(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int32(1), proxyCalls.Load())
	assert.Positive(t, client.http.Timeout)
	assert.Zero(t, supplied.Timeout)
	assert.False(t, original.DisableKeepAlives)
	assert.True(t, original.Protocols.HTTP2())
	assert.Equal(t, []string{"h2", "http/1.1"}, original.TLSClientConfig.NextProtos)
}

func TestRefreshDoesNotForwardTokenEndpointRedirect(t *testing.T) {
	var forwards atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwards.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	m := NewTokenManager(tokenTempDir(t), TokenManagerOptions{TokenURL: origin.URL})
	record := testRecord()
	require.NoError(t, m.Save(t.Context(), record))
	_, err := m.Refresh(t.Context(), record.OwnerID)
	require.Error(t, err)
	assert.Zero(t, forwards.Load())
}
