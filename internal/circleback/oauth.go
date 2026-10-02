package circleback

import (
	"log/slog"

	"go.kenn.io/msgvault/internal/mcpoauth"
)

const DefaultEndpoint = "https://circleback.ai/api/mcp"

type Manager = mcpoauth.Manager

func NewManager(endpoint, tokensDir string, logger *slog.Logger) *Manager {
	return mcpoauth.NewManager(mcpoauth.Provider{Name: "circleback", Endpoint: DefaultEndpoint, RedirectPort: "8090", CallbackPath: "/callback/circleback"}, endpoint, tokensDir, logger)
}
