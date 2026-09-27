package cmd

import (
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/vector"
)

func currentSemanticPersonVectorConfigSource(state *invocation) vector.SemanticPersonEmbeddingConfigSource {
	return func() (vector.Config, error) {
		if state == nil || state.cfg == nil {
			return vector.Config{}, errors.New("semantic person embedding runtime configuration is unavailable")
		}
		cfg := state.cfg
		configPath := strings.TrimSpace(cfg.ConfigFilePath())
		if configPath == "" {
			return vector.Config{}, errors.New("semantic person embedding runtime configuration path is unavailable")
		}
		snapshot, err := config.ReadConfigFile(configPath)
		if err != nil {
			return vector.Config{}, err
		}
		if !snapshot.Exists {
			return vector.Config{}, nil
		}
		loaded, err := config.LoadConfigFile(snapshot, cfg.HomeDir)
		if err != nil {
			return vector.Config{}, err
		}
		return loaded.Vector, nil
	}
}
