package web

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	shellCacheControl = "no-store, no-cache, must-revalidate, max-age=0"
	immutableCache    = "public, max-age=31536000, immutable"
	// img-src allows blob: for FileViewer image previews (object URLs over
	// verified archive bytes) and data: for sanitized inline images in the
	// reader's srcdoc frame, which inherits this policy. https: and http: are
	// required because sandboxed srcdoc frames enforce the intersection of
	// this policy and their own meta CSP: the frame whitelists exact remote
	// image URLs only after per-message user opt-in ("Load images"), and this
	// policy must permit those schemes for that gate to be effective. The
	// shell itself never renders unsanitized markup, so the scheme-wide
	// allowance does not expand what archived content can load. style-src-attr
	// permits inline style attributes: the reader frame needs them for
	// allowlist-sanitized author mail styling, and stylesheet elements stay
	// restricted to 'self' via style-src.
	shellCSP         = "default-src 'self'; img-src 'self' data: blob: https: http:; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
	viteManifestPath = ".vite/manifest.json"
)

// assetContentTypes pins Content-Type headers for the embedded asset
// extensions. mime.TypeByExtension consults OS-level tables (the registry on
// Windows), so relying on it would let the host system change what the
// handler serves. Values match Go's built-in table so behavior is unchanged
// on Linux and macOS.
var assetContentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".json":  "application/json",
	".mjs":   "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".woff2": "font/woff2",
}

type viteManifestEntry struct {
	File   string   `json:"file"`
	CSS    []string `json:"css"`
	Assets []string `json:"assets"`
}

// Handler serves the embedded browser application and delegates requests that
// do not belong to it to fallback.
func Handler(fallback http.Handler) http.Handler {
	assets, err := Assets()
	if err != nil {
		return fallback
	}
	return NewHandler(assets, fallback)
}

// NewHandler serves a frontend filesystem. Handler uses it with the embedded
// distribution; accepting an fs.FS also keeps routing behavior independently
// testable without requiring a frontend build.
func NewHandler(assets fs.FS, fallback http.Handler) http.Handler {
	if fallback == nil {
		fallback = http.NotFoundHandler()
	}

	index, indexErr := fs.ReadFile(assets, "index.html")
	immutableAssets := readImmutableAssets(assets)
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fallback.ServeHTTP(w, r)
			return
		}

		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "." && name != "index.html" && fs.ValidPath(name) && !forbiddenAssetPath(name) {
			info, err := fs.Stat(assets, name)
			if err == nil && !info.IsDir() {
				w.Header().Set("X-Content-Type-Options", "nosniff")
				if contentType, ok := assetContentTypes[strings.ToLower(path.Ext(name))]; ok {
					w.Header().Set("Content-Type", contentType)
				}
				if _, ok := immutableAssets[name]; ok {
					w.Header().Set("Cache-Control", immutableCache)
				}
				files.ServeHTTP(w, r)
				return
			}
		}

		if indexErr != nil || !isNavigationPath(r.URL.Path) {
			fallback.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", shellCacheControl)
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.Header().Set("Content-Security-Policy", shellCSP)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	})
}

// forbiddenAssetPath reports whether an embedded file must never be served.
// Vite copies web/public into the distribution unchanged and the embedded tree
// is served without authentication, so hidden paths (any dot-prefixed segment,
// which also covers .env and the Vite manifest) and credential-style filenames
// (client_secret*.json, oauth_client*.json, *.pem, *.key, config.toml) are
// refused even if they slip past the build-time asset validation. Blocked
// requests fall through to the SPA shell or the API fallback.
func forbiddenAssetPath(name string) bool {
	for segment := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(segment, ".") {
			return true
		}
	}
	base := strings.ToLower(path.Base(name))
	if strings.Contains(base, "client_secret") || strings.Contains(base, "oauth_client") {
		return true
	}
	if base == "config.toml" {
		return true
	}
	switch path.Ext(base) {
	case ".pem", ".key":
		return true
	}
	return false
}

func readImmutableAssets(assets fs.FS) map[string]struct{} {
	content, err := fs.ReadFile(assets, viteManifestPath)
	if err != nil {
		return nil
	}

	var manifest map[string]viteManifestEntry
	if err := json.Unmarshal(content, &manifest); err != nil {
		return nil
	}

	immutable := make(map[string]struct{})
	add := func(name string) {
		if name != "" && name != viteManifestPath && fs.ValidPath(name) {
			immutable[name] = struct{}{}
		}
	}
	for _, entry := range manifest {
		add(entry.File)
		for _, name := range entry.CSS {
			add(name)
		}
		for _, name := range entry.Assets {
			add(name)
		}
	}
	return immutable
}

func isNavigationPath(requestPath string) bool {
	if requestPath == "/api" || requestPath == "/debug" {
		return false
	}
	if requestPath == "/assets" || strings.HasPrefix(requestPath, "/assets/") {
		return false
	}
	for _, prefix := range []string{"/api/", "/debug/", "/openapi", "/docs"} {
		if strings.HasPrefix(requestPath, prefix) {
			return false
		}
	}

	cleaned := path.Clean(requestPath)
	if cleaned == "/" || cleaned == "/index.html" {
		return true
	}
	return path.Ext(cleaned) == ""
}
