// Package plaud archives recordings through Plaud's official hosted MCP.
package plaud

import (
	"log/slog"

	"go.kenn.io/msgvault/internal/mcpoauth"
)

const DefaultEndpoint = "https://mcp.plaud.ai/mcp"

type Manager = mcpoauth.Manager

func NewManager(endpoint, tokensDir string, logger *slog.Logger) *Manager {
	return mcpoauth.NewManager(mcpoauth.Provider{Name: "plaud", Endpoint: DefaultEndpoint, RedirectPort: "8091", CallbackPath: "/callback/plaud"}, endpoint, tokensDir, logger)
}
