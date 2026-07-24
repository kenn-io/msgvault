// Package web embeds and serves the browser application bundled with msgvault.
package web

import (
	"embed"
	"fmt"
	"io/fs"
)

// distFS always contains stub.html so ordinary Go builds do not require the
// frontend toolchain. Release builds copy Vite's output beside the stub before
// compiling the binary.
//
// The dist/* glob is required because, unlike the plain directory form, it
// matches top-level dot entries and therefore embeds .vite/manifest.json
// (naming dist/.vite explicitly would break Go-only builds where only the
// stub exists). The same glob would also embed a stray dist/.env or
// credential file, so 'make web-embed' validates the staged tree before every
// build and the handler refuses to serve hidden or credential-pattern paths.
//
//go:embed dist/*
var distFS embed.FS

// Assets returns the embedded frontend distribution rooted at dist/.
func Assets() (fs.FS, error) {
	assets, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, fmt.Errorf("root embedded web assets: %w", err)
	}
	return assets, nil
}
