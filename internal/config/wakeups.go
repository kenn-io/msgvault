package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/netguard"
)

type WakeupConfig struct {
	Name             string        `toml:"name"`
	URL              string        `toml:"url"`
	SecretFile       string        `toml:"secret_file"`
	SecretEnv        string        `toml:"secret_env"`
	TrustedAddresses []string      `toml:"trusted_addresses"`
	Timeout          time.Duration `toml:"timeout"`
	PersonID         int64         `toml:"person_id"`
	SourceIDs        []int64       `toml:"source_ids"`
	ConversationIDs  []int64       `toml:"conversation_ids"`
	IncomingOnly     *bool         `toml:"incoming_only"`
	MaxMessageAge    time.Duration `toml:"max_message_age"`
}

var wakeupName = regexp.MustCompile(`^[a-z0-9-]{1,48}$`)

func prepareWakeups(entries []WakeupConfig) error {
	if len(entries) > 32 {
		return fmt.Errorf("wakeups: at most 32 entries")
	}
	seen := make(map[string]bool)
	for i := range entries {
		c := &entries[i]
		if !wakeupName.MatchString(c.Name) || seen[c.Name] {
			return fmt.Errorf("wakeups: invalid or duplicate name %q", c.Name)
		}
		seen[c.Name] = true
		target, err := url.Parse(c.URL)
		if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" || target.Opaque != "" || strings.Contains(c.URL, "#") || (target.Port() != "" && target.Port() != "443" && target.Port() != "8443") {
			return fmt.Errorf("wakeups %s: invalid callback", c.Name)
		}
		if ip, e := netip.ParseAddr(target.Hostname()); e == nil {
			if netguard.ProhibitedIP(ip) {
				return fmt.Errorf("wakeups %s: prohibited callback", c.Name)
			}
		} else if netguard.ProhibitedHostname(target.Hostname()) {
			return fmt.Errorf("wakeups %s: prohibited callback", c.Name)
		}
		if len(c.TrustedAddresses) > 0 {
			addresses := make([]netip.Addr, 0, len(c.TrustedAddresses))
			for _, raw := range c.TrustedAddresses {
				ip, e := netip.ParseAddr(raw)
				if e != nil {
					return fmt.Errorf("wakeups %s: invalid trusted address", c.Name)
				}
				addresses = append(addresses, ip)
			}
			origin := *target
			origin.Path = ""
			origin.RawPath = ""
			origin.RawQuery = ""
			origin.ForceQuery = false
			if _, _, err = netguard.ValidateTrustedDestination(&origin, addresses); err != nil {
				return fmt.Errorf("wakeups %s: %w", c.Name, err)
			}
		}
		if (c.SecretFile == "") == (c.SecretEnv == "") {
			return fmt.Errorf("wakeups %s: specify exactly one secret source", c.Name)
		}
		if c.Timeout == 0 {
			c.Timeout = 10 * time.Second
		}
		if c.Timeout < 0 || c.Timeout > time.Minute {
			return fmt.Errorf("wakeups %s: invalid timeout", c.Name)
		}
		if c.MaxMessageAge == 0 {
			c.MaxMessageAge = 72 * time.Hour
		}
		if c.MaxMessageAge < 0 {
			return fmt.Errorf("wakeups %s: invalid max_message_age", c.Name)
		}
		if c.IncomingOnly == nil {
			v := true
			c.IncomingOnly = &v
		}
		if c.PersonID < 0 || len(c.SourceIDs) > 50 || len(c.ConversationIDs) > 50 {
			return fmt.Errorf("wakeups %s: invalid selectors", c.Name)
		}
		for _, ids := range [][]int64{c.SourceIDs, c.ConversationIDs} {
			for _, id := range ids {
				if id <= 0 {
					return fmt.Errorf("wakeups %s: invalid selector", c.Name)
				}
			}
		}
	}
	return nil
}
