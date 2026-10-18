package mcp

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
)

// assertMCPServerInfo checks the client-visible identity independently of the
// implementation builder and returns the image a client would decode.
func assertMCPServerInfo(t *testing.T, value any) []byte {
	t.Helper()
	info, ok := value.(map[string]any)
	require.True(t, ok, "missing serverInfo")
	assert.Equal(t, "msgvault", info["name"])
	assert.Equal(t, "1.0.0", info["version"])
	assert.Equal(t, "https://msgvault.io", info["websiteUrl"])
	icons, ok := info["icons"].([]any)
	require.True(t, ok, "missing icons")
	require.Len(t, icons, 1)
	icon, ok := icons[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "image/png", icon["mimeType"])
	assert.Equal(t, []any{"128x128"}, icon["sizes"])
	source, ok := icon["src"].(string)
	require.True(t, ok)
	const prefix = "data:image/png;base64,"
	require.True(t, strings.HasPrefix(source, prefix))
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(source, prefix))
	require.NoError(t, err)
	require.Less(t, len(data), 16*1024, "keep serverInfo small")
	img, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, 128, img.Bounds().Dx())
	assert.Equal(t, 128, img.Bounds().Dy())
	return data
}

const iconInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"synthetic-client","version":"1"}}}`

func TestMCPServerIconHTTP(t *testing.T) {
	for _, config := range []struct{ name, key string }{{"without key", ""}, {"with key", "synthetic-icon-key"}} {
		t.Run(config.name, func(t *testing.T) {
			handler := newMCPHTTPServer(ServeOptions{Engine: &querytest.MockEngine{}}, HTTPOptions{APIKey: config.key}).Handler
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(iconInitialize))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if config.key != "" {
				req.Header.Set("Authorization", "Bearer "+config.key)
			}
			recorder, response := task3Serve(handler, req)
			task3RequireSuccess(t, recorder, response)
			declared := assertMCPServerInfo(t, response.Result["serverInfo"])
			for _, path := range []string{"/icon.png", "/favicon.ico"} {
				for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
					t.Run(method+path, func(t *testing.T) {
						assert := assert.New(t)
						req := httptest.NewRequest(method, path, nil)
						req.Header.Set("Origin", "https://client.example")
						got := httptest.NewRecorder()
						handler.ServeHTTP(got, req)
						if method != http.MethodGet && method != http.MethodHead {
							assert.Equal(http.StatusMethodNotAllowed, got.Code)
							assert.Equal("GET, HEAD", got.Header().Get("Allow"))
							return
						}
						require.Equal(t, http.StatusOK, got.Code)
						assert.Equal("image/png", got.Header().Get("Content-Type"))
						assert.Equal("nosniff", got.Header().Get("X-Content-Type-Options"))
						assert.Equal("public, max-age=86400", got.Header().Get("Cache-Control"))
						assert.Equal(strconv.Itoa(len(declared)), got.Header().Get("Content-Length"))
						assert.Empty(got.Header().Get("WWW-Authenticate"))
						if method == http.MethodHead {
							assert.Empty(got.Body.Bytes())
						} else {
							assert.Equal(declared, got.Body.Bytes())
						}
					})
				}
			}
			if config.key != "" {
				got := httptest.NewRecorder()
				handler.ServeHTTP(got, httptest.NewRequest(http.MethodPost, "/mcp", nil))
				assert.Equal(t, http.StatusUnauthorized, got.Code)
				assert.Equal(t, "no-store", got.Header().Get("Cache-Control"))
			}
			got := httptest.NewRecorder()
			handler.ServeHTTP(got, httptest.NewRequest(http.MethodGet, "/unknown-icon", nil))
			assert.Equal(t, http.StatusNotFound, got.Code)
		})
	}
}
