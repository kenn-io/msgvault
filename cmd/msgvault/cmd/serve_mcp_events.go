package cmd

import (
	"context"
	"path/filepath"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/store"
)

func newDaemonMCPEventsService(ctx context.Context, cfg *config.Config, st *store.Store, server *api.Server) (*mcpevents.Service, error) {
	retention, err := cfg.MCP.Events.RetentionDuration()
	if err != nil {
		return nil, err
	}
	trusted := make([]mcpevents.TrustedCallback, 0, len(cfg.MCP.Events.TrustedCallbacks))
	for _, entry := range cfg.MCP.Events.TrustedCallbacks {
		trusted = append(trusted, mcpevents.TrustedCallback{Origin: entry.Origin, Addresses: append([]string(nil), entry.Addresses...)})
	}
	return mcpevents.New(ctx, st, mcpevents.Options{
		Enabled:          cfg.MCP.Events.Enabled,
		Retention:        retention,
		Sources:          append([]string(nil), cfg.MCP.Events.Sources...),
		TrustedCallbacks: trusted,
		KeyPath:          filepath.Join(cfg.Data.DataDir, "mcp-events.key"),
		OwnerKey:         cfg.Server.AuthenticationKey(),
		WithOperation:    server.MCPEventsOperation,
	})
}
