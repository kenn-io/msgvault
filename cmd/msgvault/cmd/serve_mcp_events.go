package cmd

import (
	"context"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/mcpevents"
	"go.kenn.io/msgvault/internal/store"
)

func newDaemonMCPEventsService(ctx context.Context, cfg *config.Config, st *store.Store, server *api.Server) (*mcpevents.Service, error) {
	opts, err := daemonMCPEventsOptions(cfg)
	if err != nil {
		return nil, err
	}
	opts.WithOperation = server.MCPEventsOperation
	opts.WithDeliveryOperation = server.MCPEventsDeliveryOperation
	return mcpevents.New(ctx, st, opts)
}

func configureDaemonMCPEventsCapture(ctx context.Context, st *store.Store) error {
	captureConfig, err := decodeDaemonMCPEventsCaptureConfig(os.Getenv(daemonMCPEventsCaptureConfigEnv))
	if err != nil {
		return err
	}
	_, err = st.ConfigureMCPEvents(ctx, captureConfig)
	return err
}

func daemonMCPEventsStoreConfig(cfg *config.Config) (store.MCPEventsConfig, error) {
	opts, err := daemonMCPEventsOptions(cfg)
	if err != nil {
		return store.MCPEventsConfig{}, err
	}
	return mcpevents.StoreCaptureConfig(opts), nil
}

func daemonMCPEventsOptions(cfg *config.Config) (mcpevents.Options, error) {
	retention, err := cfg.MCP.Events.RetentionDuration()
	if err != nil {
		return mcpevents.Options{}, err
	}
	trusted := make([]mcpevents.TrustedCallback, 0, len(cfg.MCP.Events.TrustedCallbacks))
	for _, entry := range cfg.MCP.Events.TrustedCallbacks {
		trusted = append(trusted, mcpevents.TrustedCallback{Origin: entry.Origin, Addresses: append([]string(nil), entry.Addresses...)})
	}
	return mcpevents.Options{
		Enabled:          cfg.MCP.Events.Enabled,
		Retention:        retention,
		Sources:          append([]string(nil), cfg.MCP.Events.Sources...),
		TrustedCallbacks: trusted,
		KeyPath:          filepath.Join(cfg.Data.DataDir, "mcp-events.key"),
		OwnerKey:         cfg.Server.AuthenticationKey(),
	}, nil
}
