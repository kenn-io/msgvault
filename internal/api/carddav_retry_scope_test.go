package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCardDAVNamedDiscoveryRetryHeaderIgnoresDefaultGate(t *testing.T) {
	for _, multistatus := range []bool{false, true} {
		name := "http_429"
		if multistatus {
			name = "multistatus_429"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			controller, baseURL := multipleCardDAVController(t)
			_, err := controller.Save(t.Context(), CardDAVAccountRequest{
				BaseURL: baseURL, Username: "default", Password: "default-synthetic-secret", Enabled: new(true),
			})
			require.NoError(err)
			require.NoError(controller.store.SetCardDAVRetryAfterContext(t.Context(), time.Now().Add(5*time.Minute), store.DefaultCardDAVAccountID))

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "PROPFIND", r.Method)
				if multistatus {
					writeCardDAVMultiStatus(w, `<D:response><D:href>/dav</D:href><D:status>HTTP/1.1 429 Too Many Requests</D:status></D:response>`)
				} else {
					w.WriteHeader(http.StatusTooManyRequests)
				}
			}))
			t.Cleanup(upstream.Close)
			target, err := url.Parse(upstream.URL)
			require.NoError(err)
			controller.factory = fixtureCardDAVFactory(t, target)
			server := cardDAVReadServer(t, controller.cfg, controller, nil)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/carddav/account/test", strings.NewReader(
				`{"connection":"work","base_url":"`+baseURL+`","username":"work","password":"work-synthetic-secret","enabled":true}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, request)
			require.Equal(http.StatusServiceUnavailable, response.Code, response.Body.String())
			assert.Equal(t, "1", response.Header().Get("Retry-After"), "the provider supplied no delay; the default account's gate is unrelated")
		})
	}
}
