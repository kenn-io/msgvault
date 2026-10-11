package archive_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/archive"
)

func TestLibraryHandlerCallerPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		origin     string
		allowed    bool
		apiKey     string
		cache      string
		wantStatus int
		wantCache  string
	}{
		{name: "authorized read", allowed: true, wantStatus: http.StatusOK, wantCache: "no-store"},
		{name: "rejected read", wantStatus: http.StatusForbidden, wantCache: "no-store"},
		{name: "same origin", origin: "https://archive.example", allowed: true, wantStatus: http.StatusOK, wantCache: "no-store"},
		{name: "caller allows cross origin", origin: "https://client.example", allowed: true, wantStatus: http.StatusOK, wantCache: "no-store"},
		{name: "caller rejects cross origin", origin: "https://client.example", wantStatus: http.StatusForbidden, wantCache: "no-store"},
		{name: "daemon key does not replace caller policy", allowed: true, apiKey: "synthetic-daemon-key", wantStatus: http.StatusOK, wantCache: "no-store"},
		{name: "caller overrides cache policy", allowed: true, cache: "private, max-age=0", wantStatus: http.StatusOK, wantCache: "private, max-age=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			cfg := &archive.Config{}
			cfg.Server.APIKey = tc.apiKey
			srv := archive.NewServer(archive.ServerOptions{Config: cfg, Store: st})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			handler := srv.Handler(func(w http.ResponseWriter, _ *http.Request, op archive.Operation) bool {
				assert.Equal("GET", op.Method)
				assert.Equal("/api/v1/messages/changes", op.Path)
				if tc.cache != "" {
					w.Header().Set("Cache-Control", tc.cache)
				}
				if !tc.allowed {
					w.WriteHeader(http.StatusForbidden)
				}
				return tc.allowed
			})
			r := httptest.NewRequest(http.MethodGet, "https://archive.example/api/v1/messages/changes", nil)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(tc.wantStatus, w.Code, w.Body.String())
			assert.Equal(tc.wantCache, w.Header().Get("Cache-Control"))
			if tc.allowed {
				assert.Contains(w.Body.String(), `"messages":[]`)
			} else {
				assert.Empty(w.Body.String())
			}
		})
	}
}
