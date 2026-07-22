package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testIndex = `<!doctype html><html><head><title>test shell</title></head><body>app</body></html>`

const testViteManifest = `{
  "index.html": {
    "file": "assets/index-DV9xHoWC.js",
    "name": "index",
    "src": "index.html",
    "isEntry": true,
    "css": ["assets/index-DZ2EoQeD.css"],
    "assets": ["assets/logo-horizontal.svg"]
  },
  "src/lib/conversation.ts": {
    "file": "assets/conversation-DV9xHoWC.js",
    "name": "conversation",
    "src": "src/lib/conversation.ts",
    "isDynamicEntry": true
  }
}`

func TestWebHandlerServesExactAssetsBeforeNavigationFallback(t *testing.T) {
	assets := fstest.MapFS{
		"index.html":                      {Data: []byte(testIndex)},
		".vite/manifest.json":             {Data: []byte(testViteManifest)},
		"assets/index-DV9xHoWC.js":        {Data: []byte(`console.log("bundle")`)},
		"assets/index-DZ2EoQeD.css":       {Data: []byte(`body { color: black; }`)},
		"assets/conversation-DV9xHoWC.js": {Data: []byte(`export const view = {}`)},
		"assets/app-release1.js":          {Data: []byte(`console.log("release")`)},
		"assets/theme-variant2.css":       {Data: []byte(`:root { color-scheme: light; }`)},
		"assets/unversioned-helper.js":    {Data: []byte(`export {}`)},
		"assets/logo-horizontal.svg":      {Data: []byte(`<svg></svg>`)},
		"assets/manifest-production.json": {Data: []byte(`{}`)},
		"assets/theme-darkmode.css":       {Data: []byte(`:root { color-scheme: dark; }`)},
	}
	handler := NewHandler(assets, jsonNotFoundHandler())

	cases := []struct {
		name      string
		path      string
		wantBody  string
		wantCache string
	}{
		{
			name:      "hashed JavaScript",
			path:      "/assets/index-DV9xHoWC.js",
			wantBody:  `console.log("bundle")`,
			wantCache: "public, max-age=31536000, immutable",
		},
		{
			name:      "hashed stylesheet",
			path:      "/assets/index-DZ2EoQeD.css",
			wantBody:  `body { color: black; }`,
			wantCache: "public, max-age=31536000, immutable",
		},
		{
			name:      "hashed named chunk",
			path:      "/assets/conversation-DV9xHoWC.js",
			wantBody:  `export const view = {}`,
			wantCache: "public, max-age=31536000, immutable",
		},
		{
			name:     "plausible mutable JavaScript",
			path:     "/assets/app-release1.js",
			wantBody: `console.log("release")`,
		},
		{
			name:     "plausible mutable stylesheet",
			path:     "/assets/theme-variant2.css",
			wantBody: `:root { color-scheme: light; }`,
		},
		{
			name:     "unversioned asset",
			path:     "/assets/unversioned-helper.js",
			wantBody: `export {}`,
		},
		{
			name:      "manifest-declared image",
			path:      "/assets/logo-horizontal.svg",
			wantBody:  `<svg></svg>`,
			wantCache: "public, max-age=31536000, immutable",
		},
		{
			name:     "environment-specific manifest",
			path:     "/assets/manifest-production.json",
			wantBody: `{}`,
		},
		{
			name:     "eight-character descriptive suffix",
			path:     "/assets/theme-darkmode.css",
			wantBody: `:root { color-scheme: dark; }`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))

			assert.Equal(http.StatusOK, recorder.Code)
			assert.Equal(tc.wantBody, recorder.Body.String())
			assert.Equal(tc.wantCache, recorder.Header().Get("Cache-Control"))
			assert.Equal("nosniff", recorder.Header().Get("X-Content-Type-Options"))
			assert.Empty(recorder.Header().Get("Content-Security-Policy"))
		})
	}
}

func TestWebHandlerServesShellForSafeNavigation(t *testing.T) {
	handler := NewHandler(fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(testIndex)},
	}, jsonNotFoundHandler())

	for _, path := range []string{"/", "/index.html", "/not/a/real/route"} {
		t.Run(path, func(t *testing.T) {
			assert := assert.New(t)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

			assert.Equal(http.StatusOK, recorder.Code)
			assert.Equal(testIndex, recorder.Body.String())
			assert.Equal("text/html; charset=utf-8", recorder.Header().Get("Content-Type"))
			assert.Equal("no-store, no-cache, must-revalidate, max-age=0", recorder.Header().Get("Cache-Control"))
			assert.Equal("no-cache", recorder.Header().Get("Pragma"))
			assert.Equal("0", recorder.Header().Get("Expires"))
			assert.Equal("nosniff", recorder.Header().Get("X-Content-Type-Options"))
			assert.Equal(
				"default-src 'self'; img-src 'self' data: blob: https: http:; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'",
				recorder.Header().Get("Content-Security-Policy"),
			)
		})
	}
}

func TestWebHandlerDelegatesUnsafeFallbacks(t *testing.T) {
	handler := NewHandler(fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(testIndex)},
	}, jsonNotFoundHandler())

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{name: "bare API root", method: http.MethodGet, path: "/api"},
		{name: "unknown API", method: http.MethodGet, path: "/api/v1/not-real"},
		{name: "bare debug root", method: http.MethodGet, path: "/debug"},
		{name: "debug route", method: http.MethodGet, path: "/debug/not-real"},
		{name: "OpenAPI route", method: http.MethodGet, path: "/openapi-not-real"},
		{name: "docs route", method: http.MethodGet, path: "/docs/not-real"},
		{name: "missing extensionless asset", method: http.MethodGet, path: "/assets/missing"},
		{name: "missing exact asset", method: http.MethodGet, path: "/assets/index-MISSING.js"},
		{name: "non-navigation method", method: http.MethodPost, path: "/not/a/real/route"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))

			assert.Equal(http.StatusNotFound, recorder.Code)
			assert.Equal("application/json", recorder.Header().Get("Content-Type"))
			assert.Empty(recorder.Header().Get("Content-Security-Policy"))
			var response map[string]string
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal("not_found", response["error"])
		})
	}
}

func TestWebHandlerSupportsHEADWithoutResponseBodies(t *testing.T) {
	handler := NewHandler(fstest.MapFS{
		"index.html":               {Data: []byte(testIndex)},
		".vite/manifest.json":      {Data: []byte(testViteManifest)},
		"assets/index-DV9xHoWC.js": {Data: []byte(`console.log("bundle")`)},
	}, jsonNotFoundHandler())

	cases := []struct {
		name       string
		path       string
		wantType   string
		wantCache  string
		wantLength string
	}{
		{
			name:       "shell",
			path:       "/everything",
			wantType:   "text/html; charset=utf-8",
			wantCache:  "no-store, no-cache, must-revalidate, max-age=0",
			wantLength: "82",
		},
		{
			name:       "asset",
			path:       "/assets/index-DV9xHoWC.js",
			wantType:   "text/javascript; charset=utf-8",
			wantCache:  "public, max-age=31536000, immutable",
			wantLength: "21",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, tc.path, nil))

			assert.Equal(http.StatusOK, recorder.Code)
			assert.Empty(recorder.Body.String())
			assert.Equal(tc.wantType, recorder.Header().Get("Content-Type"))
			assert.Equal(tc.wantCache, recorder.Header().Get("Cache-Control"))
			assert.Equal(tc.wantLength, recorder.Header().Get("Content-Length"))
			assert.Equal("nosniff", recorder.Header().Get("X-Content-Type-Options"))
		})
	}
}

func TestWebHandlerFailsConservativelyWithoutValidManifest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		manifest *fstest.MapFile
	}{
		{name: "missing manifest"},
		{name: "malformed manifest", manifest: &fstest.MapFile{Data: []byte(`{"index.html":`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			assets := fstest.MapFS{
				"index.html":               {Data: []byte(testIndex)},
				"assets/index-DV9xHoWC.js": {Data: []byte(`console.log("bundle")`)},
			}
			if tc.manifest != nil {
				assets[".vite/manifest.json"] = tc.manifest
			}
			handler := NewHandler(assets, jsonNotFoundHandler())

			assetRecorder := httptest.NewRecorder()
			handler.ServeHTTP(assetRecorder, httptest.NewRequest(http.MethodGet, "/assets/index-DV9xHoWC.js", nil))
			assertions.Equal(http.StatusOK, assetRecorder.Code)
			assertions.Empty(assetRecorder.Header().Get("Cache-Control"))

			shellRecorder := httptest.NewRecorder()
			handler.ServeHTTP(shellRecorder, httptest.NewRequest(http.MethodGet, "/not/a/real/route", nil))
			assertions.Equal(http.StatusOK, shellRecorder.Code)
			assertions.Equal(testIndex, shellRecorder.Body.String())
		})
	}
}

func TestWebHandlerDoesNotExposeViteManifest(t *testing.T) {
	handler := NewHandler(fstest.MapFS{
		"index.html":          {Data: []byte(testIndex)},
		".vite/manifest.json": {Data: []byte(testViteManifest)},
	}, jsonNotFoundHandler())
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/.vite/manifest.json", nil))

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
	assert.NotContains(t, recorder.Body.String(), "DV9xHoWC")
}

func TestWebHandlerDelegatesWhenEmbeddedIndexIsAbsent(t *testing.T) {
	handler := NewHandler(fstest.MapFS{
		"stub.html": &fstest.MapFile{Data: []byte("ok\n")},
	}, jsonNotFoundHandler())
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusNotFound, recorder.Code)
	assert.Equal(t, "application/json", recorder.Header().Get("Content-Type"))
	assert.NotContains(t, recorder.Body.String(), "stub")
}

func TestWebEmbeddedAssetsIncludeCompilationStub(t *testing.T) {
	assets, err := Assets()
	require.NoError(t, err)

	content, err := fs.ReadFile(assets, "stub.html")
	require.NoError(t, err)
	assert.Equal(t, "ok\n", string(content))
}

func jsonNotFoundHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "not_found",
			"message": "route not found",
		})
	})
}
