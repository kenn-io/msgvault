package cmd

import (
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/requestsign"
)

func configuredRemoteClientConfig(cfg *config.Config) (daemonclient.Config, error) {
	r := cfg.Remote
	if err := r.Validate(); err != nil {
		return daemonclient.Config{}, err
	}
	c := daemonclient.Config{URL: r.URL, APIKey: r.APIKey, AllowInsecure: r.AllowInsecure, SigningKeyID: r.SigningKeyID, MaxRequestBytes: r.MaxRequestBytes}
	var err error
	if r.APIKeyFile != "" {
		c.APIKey, err = requestsign.ReadAPIKey(r.APIKeyFile)
		if err != nil {
			return daemonclient.Config{}, err
		}
	}
	if r.SigningEnabled() {
		c.SigningSecret, err = requestsign.ReadSigningSecret(r.SigningSecretFile)
		if err != nil {
			return daemonclient.Config{}, err
		}
	}
	return c, nil
}
