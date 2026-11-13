package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/meta"
)

// MetaWebhooksConfig starts a separate listener for signed Meta webhook
// deliveries. Secrets exist only in the named daemon environment variables.
type MetaWebhooksConfig struct {
	ListenAddr string           `toml:"listen_addr"`
	Apps       []MetaWebhookApp `toml:"apps"`
}

type MetaWebhookApp struct {
	AppID          string `toml:"app_id"`
	AppSecretEnv   string `toml:"app_secret_env"`
	VerifyTokenEnv string `toml:"verify_token_env"`
}

func (c *Config) validateMetaWebhooks() error {
	cfg := c.MetaWebhooks
	if cfg.ListenAddr == "" {
		if len(cfg.Apps) > 0 {
			return errors.New("meta_webhooks.listen_addr is required with apps")
		}
		return nil
	}
	_, port, err := net.SplitHostPort(cfg.ListenAddr)
	p, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || p <= 0 || p > 65535 {
		return errors.New("meta_webhooks.listen_addr must be host:port with a port from 1 to 65535")
	}
	if len(cfg.Apps) == 0 {
		return errors.New("meta_webhooks.apps is required with listen_addr")
	}
	seen := map[string]bool{}
	for i, app := range cfg.Apps {
		if !meta.ValidAccountID(app.AppID) || seen[app.AppID] {
			return fmt.Errorf("meta_webhooks.apps[%d].app_id must be a unique decimal app ID", i)
		}
		seen[app.AppID] = true
		if !envutil.ValidName(app.AppSecretEnv) || !envutil.ValidName(app.VerifyTokenEnv) {
			return fmt.Errorf("meta_webhooks.apps[%d] needs app_secret_env and verify_token_env naming environment variables", i)
		}
	}
	return nil
}
