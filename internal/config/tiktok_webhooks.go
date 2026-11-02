package config

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/tiktok"
)

type TikTokWebhooksConfig struct {
	ListenAddr    string             `toml:"listen_addr"`
	MaxBodyBytes  int64              `toml:"max_body_bytes"`
	MaxSpoolBytes int64              `toml:"max_spool_bytes"`
	DoneRetention string             `toml:"done_retention"`
	Apps          []TikTokWebhookApp `toml:"apps"`
}
type TikTokWebhookApp struct {
	AppID        string `toml:"app_id"`
	AppSecretEnv string `toml:"app_secret_env"`
}

func (c TikTokWebhooksConfig) Enabled() bool { return c.ListenAddr != "" && len(c.Apps) > 0 }
func (c TikTokWebhooksConfig) Effective() (body, spool int64, retention time.Duration) {
	body, spool, retention = c.MaxBodyBytes, c.MaxSpoolBytes, 30*24*time.Hour
	if body == 0 {
		body = 1 << 20
	}
	if spool == 0 {
		spool = 128 << 20
	}
	if c.DoneRetention != "" {
		retention, _ = time.ParseDuration(c.DoneRetention)
	}
	return body, spool, retention
}
func (c TikTokWebhooksConfig) Validate() error {
	body, spool, retention := c.Effective()
	if body <= 0 || body > 8<<20 {
		return fmt.Errorf("tiktok_webhooks.max_body_bytes must be between 1 and 8388608")
	}
	if spool < 2*body+4096 {
		return fmt.Errorf("tiktok_webhooks.max_spool_bytes must be at least twice the body limit plus 4096")
	}
	if retention <= 0 {
		return fmt.Errorf("tiktok_webhooks.done_retention must be a positive duration")
	}
	if c.ListenAddr == "" && len(c.Apps) == 0 {
		return nil
	}
	_, port, err := net.SplitHostPort(c.ListenAddr)
	n, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || n < 1 || n > 65535 {
		return fmt.Errorf("tiktok_webhooks.listen_addr must be host:port with port 1 through 65535")
	}
	if len(c.Apps) == 0 {
		return fmt.Errorf("tiktok_webhooks.apps is required with listen_addr")
	}
	seen := map[string]bool{}
	for i, app := range c.Apps {
		if !tiktok.ValidID(app.AppID) || seen[app.AppID] {
			return fmt.Errorf("tiktok_webhooks.apps[%d].app_id is empty or duplicate", i)
		}
		seen[app.AppID] = true
		if !envutil.ValidName(app.AppSecretEnv) {
			return fmt.Errorf("tiktok_webhooks.apps[%d].app_secret_env must name an explicit environment variable", i)
		}
	}
	return nil
}
