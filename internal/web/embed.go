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
