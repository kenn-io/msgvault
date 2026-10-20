package cmd

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
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

// requireDaemonConfig rejects settings that client commands only warn about
// but a daemon cannot start with.
func requireDaemonConfig(cfg *config.Config) error {
	if err := cfg.DaemonConfigError(); err != nil {
		return fmt.Errorf("daemon config invalid: %w", err)
	}
	return nil
}

// warnDaemonConfigErrors tells a client command about invalid daemon-only
// settings. The daemon's own CLI children stay quiet; their parent warned.
func warnDaemonConfigErrors(cmd *cobra.Command, cfg *config.Config) {
	if isDaemonConsoleSubprocess() {
		return
	}
	for _, err := range cfg.DaemonConfigErrors() {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: invalid daemon config: %v\n", err)
	}
}

func prepareServeConfig(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if err := requireDaemonConfig(cfg); err != nil {
		return err
	}
	if _, err := cfg.ResolveServerBindAddress(); err != nil {
		return err
	}
	return cfg.ValidateServerKey()
}

// daemonRuntimeChildEnv keeps serve flags effective when children reload config.
func daemonRuntimeChildEnv(ctx context.Context, env []string) []string {
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
