package mcp

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"net/http"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverIconPNG is msgvault's own website/favicon.svg rendered at 128x128.
// Regenerate from the repository root with:
//
// rsvg-convert -w 128 -h 128 website/favicon.svg -o /tmp/msgvault-icon.png
// magick /tmp/msgvault-icon.png -strip -define png:exclude-chunks=date,time internal/mcp/assets/icon.png
//
//go:embed assets/icon.png
var serverIconPNG []byte

// A data URI works for stdio and HTTP without a deployment-specific public URL.
var serverIconSource = "data:image/png;base64," + base64.StdEncoding.EncodeToString(serverIconPNG)

func serverImplementation() *sdkmcp.Implementation {
	return &sdkmcp.Implementation{
		Name:       "msgvault",
		Version:    "1.0.0",
		WebsiteURL: "https://msgvault.io",
		Icons: []sdkmcp.Icon{{
			Source:   serverIconSource,
			MIMEType: "image/png",
			Sizes:    []string{"128x128"},
		}},
	}
}

// registerIconRoutes exposes only the public mark, outside MCP authentication.
// Both aliases serve the PNG declared in serverInfo, including /favicon.ico.
func registerIconRoutes(mux *http.ServeMux) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeContent(w, r, "icon.png", time.Time{}, bytes.NewReader(serverIconPNG))
	})
	mux.Handle("GET /icon.png", handler)
	mux.Handle("GET /favicon.ico", handler)
}
