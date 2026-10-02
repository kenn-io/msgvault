package cmd

import (
	"errors"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
)

func addServeConfigFlags(cmd *cobra.Command) {
	cmd.Flags().String("bind", "", "Bind address or iface:NAME (overrides environment and config)")
	cmd.Flags().Int("port", 0, "HTTP API port (0 chooses an open port; overrides environment and config)")
}

func serveBindSource(cmd *cobra.Command, cfg *config.Config) string {
	if flag := cmd.Flags().Lookup("bind"); flag != nil && flag.Changed {
		return "--bind"
	}
	if _, present := os.LookupEnv("MSGVAULT_BIND_ADDR"); present {
		return "MSGVAULT_BIND_ADDR"
	}
	if _, err := os.Stat(cfg.ConfigFilePath()); err == nil {
		return cfg.ConfigFilePath()
	}
	return "default"
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
	return cfg.PrepareServerKey()
}
