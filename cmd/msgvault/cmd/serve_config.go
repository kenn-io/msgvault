package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/line"
)

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

const lineActiveChannelsEnv = "MSGVAULT_INTERNAL_LINE_ACTIVE_CHANNELS"

type lineActiveChannelsContextKey struct{}

func withLineActiveChannels(ctx context.Context, active []line.ActiveChannel) context.Context {
	return context.WithValue(ctx, lineActiveChannelsContextKey{}, append([]line.ActiveChannel(nil), active...))
}

// daemonRuntimeChildEnv keeps serve flags effective when children reload config.
// The private receiver snapshot is authoritative even without invocation state;
// remove every inherited/request spelling before appending the parent value.
func daemonRuntimeChildEnv(ctx context.Context, env []string) []string {
	clean := make([]string, 0, len(env)+3)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(name, lineActiveChannelsEnv) {
			clean = append(clean, entry)
		}
	}
	active := []line.ActiveChannel{}
	if ctx != nil {
		if snapshot, ok := ctx.Value(lineActiveChannelsContextKey{}).([]line.ActiveChannel); ok && len(snapshot) > 0 {
			active = append(active, snapshot...)
		}
	}
	encoded, err := json.Marshal(active)
	if err != nil {
		// An unavailable snapshot must never authorize remote webhook changes.
		encoded = []byte("[]")
	}
	clean = append(clean, lineActiveChannelsEnv+"="+string(encoded))
	env = clean
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return env
	}
	bind := state.cfg.Server.BindAddr
	if bind == "" {
		bind = defaultDaemonBindAddr
	}
	// exec.Cmd uses the last value for a duplicate environment key.
	return append(env,
		"MSGVAULT_BIND_ADDR="+bind,
		"MSGVAULT_API_PORT="+strconv.Itoa(state.cfg.Server.APIPort),
	)
}
