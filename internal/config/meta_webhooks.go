package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/meta"
)

type MetaWebhooksConfig struct {
	ListenAddr    string           `toml:"listen_addr"`
	MaxBodyBytes  int64            `toml:"max_body_bytes"`
	MaxSpoolBytes int64            `toml:"max_spool_bytes"`
	DoneRetention string           `toml:"done_retention"`
	Apps          []MetaWebhookApp `toml:"apps"`
}
type MetaWebhookApp struct {
	AppID          string `toml:"app_id"`
	AppSecretEnv   string `toml:"app_secret_env"`
	VerifyTokenEnv string `toml:"verify_token_env"`
}

func (c MetaWebhooksConfig) Enabled() bool { return c.ListenAddr != "" && len(c.Apps) > 0 }
func (c MetaWebhooksConfig) Effective() (bodyCap, spoolCap int64, retention time.Duration) {
	bodyCap, spoolCap = c.MaxBodyBytes, c.MaxSpoolBytes
	if bodyCap == 0 {
		bodyCap = 8388608
	}
	if spoolCap == 0 {
		spoolCap = 1073741824
	}
	retention = 720 * time.Hour
	if c.DoneRetention != "" {
		retention, _ = time.ParseDuration(c.DoneRetention)
	}
	return bodyCap, spoolCap, retention
}
func (c *Config) validateMetaWebhooks() error {
	cfg := c.MetaWebhooks
	if cfg.MaxBodyBytes <= 0 {
		return errors.New("meta_webhooks.max_body_bytes must be positive")
	}
	if cfg.MaxSpoolBytes <= 0 {
		return errors.New("meta_webhooks.max_spool_bytes must be positive")
	}
	if retention, err := time.ParseDuration(cfg.DoneRetention); err != nil || retention <= 0 {
		return errors.New("meta_webhooks.done_retention must be a positive duration")
	}
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
	if cfg.ListenAddr == net.JoinHostPort(c.Server.BindAddr, strconv.Itoa(c.Server.APIPort)) {
		return errors.New("meta_webhooks.listen_addr must differ from server address")
	}
	if len(cfg.Apps) == 0 {
		return errors.New("meta_webhooks.apps is required with listen_addr")
	}
	seen := map[string]bool{}
	for i, app := range cfg.Apps {
		if !meta.ValidAccountID(app.AppID) {
			return fmt.Errorf("meta_webhooks.apps[%d].app_id must be decimal", i)
		}
		if seen[app.AppID] {
			return fmt.Errorf("meta_webhooks.apps[%d].app_id is duplicate", i)
		}
		seen[app.AppID] = true
		if !envutil.ValidName(app.AppSecretEnv) {
			return fmt.Errorf("meta_webhooks.apps[%d].app_secret_env must name an environment variable", i)
		}
		if !envutil.ValidName(app.VerifyTokenEnv) {
			return fmt.Errorf("meta_webhooks.apps[%d].verify_token_env must name an environment variable", i)
		}
	}
	return nil
}
