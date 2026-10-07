package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

const daemonMCPEventsCaptureEnv = "MSGVAULT_DAEMON_MCP_EVENTS_CAPTURE"
const daemonMCPEventsCaptureConfigEnv = "MSGVAULT_DAEMON_MCP_EVENTS_CAPTURE_CONFIG"

type daemonMCPEventsCaptureConfigJSON struct {
	Enabled      bool                           `json:"enabled"`
	Principal    string                         `json:"principal"`
	Capabilities []daemonMCPEventCapabilityJSON `json:"capabilities"`
	Retention    time.Duration                  `json:"retention"`
}

type daemonMCPEventCapabilityJSON struct {
	Family     string   `json:"family"`
	SourceType string   `json:"source_type"`
	Kinds      []string `json:"kinds"`
}

func addServeConfigFlags(cmd *cobra.Command) {
	cmd.Flags().String("bind", "", "Bind address or iface:NAME (overrides environment and config)")
	cmd.Flags().Int("port", 0, "HTTP API port (0 chooses an open port; overrides environment and config)")
}

func serveRuntimeOverrides(cmd *cobra.Command) config.RuntimeOverrides {
	var overrides config.RuntimeOverrides
	if cmd.Name() != "serve" {
		return overrides
	}
	if flag := cmd.Flags().Lookup("bind"); flag != nil && flag.Changed {
		value, _ := cmd.Flags().GetString("bind") // Cobra has validated the declared string flag.
		overrides.BindAddr = &value
	}
	if flag := cmd.Flags().Lookup("port"); flag != nil && flag.Changed {
		value, _ := cmd.Flags().GetInt("port") // Cobra has validated the declared integer flag.
		overrides.APIPort = &value
	}
	return overrides
}

func resolveServeBind(address string) (string, error) {
	return config.ResolveBindAddress(address)
}

func prepareServeConfig(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if _, err := cfg.ResolveServerBindAddress(); err != nil {
		return err
	}
	return cfg.ValidateServerKey()
}

// daemonRuntimeChildEnv keeps serve flags effective when children reload config.
func daemonRuntimeChildEnv(ctx context.Context, env []string) []string {
	state := invocationFromContext(ctx)
	out := make([]string, 0, len(env)+3)
	for _, entry := range env {
		if key, ok := splitEnvEntry(entry); ok {
			if strings.EqualFold(key, daemonMCPEventsCaptureEnv) || strings.EqualFold(key, daemonMCPEventsCaptureConfigEnv) {
				continue
			}
		}
		out = append(out, entry)
	}
	if state == nil || state.cfg == nil {
		return out
	}
	bind := state.cfg.Server.BindAddr
	if bind == "" {
		bind = defaultDaemonBindAddr
	}
	// exec.Cmd uses the last value for a duplicate environment key.
	out = append(out,
		"MSGVAULT_BIND_ADDR="+bind,
		"MSGVAULT_API_PORT="+strconv.Itoa(state.cfg.Server.APIPort),
	)
	if state.mcpEventsCapture {
		out = append(out, daemonMCPEventsCaptureEnv+"=1")
		if state.mcpEventsCaptureConfig != nil {
			encoded, err := encodeDaemonMCPEventsCaptureConfig(*state.mcpEventsCaptureConfig)
			if err == nil {
				out = append(out, daemonMCPEventsCaptureConfigEnv+"="+encoded)
			} else {
				// Leave a missing snapshot for the child to reject before ingest.
				out = append(out, daemonMCPEventsCaptureConfigEnv+"=")
			}
		}
	}
	return out
}

func encodeDaemonMCPEventsCaptureConfig(cfg store.MCPEventsConfig) (string, error) {
	wire := daemonMCPEventsCaptureConfigJSON{
		Enabled:   cfg.Enabled,
		Principal: cfg.Principal,
		Retention: cfg.Retention,
	}
	wire.Capabilities = make([]daemonMCPEventCapabilityJSON, len(cfg.Capabilities))
	for i, capability := range cfg.Capabilities {
		wire.Capabilities[i] = daemonMCPEventCapabilityJSON{
			Family:     capability.Family,
			SourceType: capability.SourceType,
			Kinds:      append([]string(nil), capability.Kinds...),
		}
	}
	payload, err := json.Marshal(wire)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeDaemonMCPEventsCaptureConfig(encoded string) (store.MCPEventsConfig, error) {
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return store.MCPEventsConfig{}, errors.New("invalid daemon MCP Events capture configuration")
	}
	var wire daemonMCPEventsCaptureConfigJSON
	if err := json.Unmarshal(payload, &wire); err != nil || !wire.Enabled || wire.Principal == "" {
		return store.MCPEventsConfig{}, errors.New("invalid daemon MCP Events capture configuration")
	}
	cfg := store.MCPEventsConfig{
		Enabled:   wire.Enabled,
		Principal: wire.Principal,
		Retention: wire.Retention,
	}
	cfg.Capabilities = make([]store.MCPEventCapability, len(wire.Capabilities))
	for i, capability := range wire.Capabilities {
		cfg.Capabilities[i] = store.MCPEventCapability{
			Family:     capability.Family,
			SourceType: capability.SourceType,
			Kinds:      append([]string(nil), capability.Kinds...),
		}
	}
	return cfg, nil
}

func cloneMCPEventsConfig(cfg store.MCPEventsConfig) store.MCPEventsConfig {
	cfg.Capabilities = append([]store.MCPEventCapability(nil), cfg.Capabilities...)
	for i := range cfg.Capabilities {
		cfg.Capabilities[i].Kinds = append([]string(nil), cfg.Capabilities[i].Kinds...)
	}
	return cfg
}
