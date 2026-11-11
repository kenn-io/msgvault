package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
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
	PersonID         *int64        `toml:"person_id"`
	SourceIDs        []int64       `toml:"source_ids"`
	ConversationIDs  []int64       `toml:"conversation_ids"`
	IncomingOnly     *bool         `toml:"incoming_only"`
	MaxMessageAge    time.Duration `toml:"max_message_age"`
}

var wakeupName = regexp.MustCompile(`^[a-z0-9-]{1,48}$`)

func prepareWakeups(entries []WakeupConfig) error {
	if len(entries) > 32 {
		return errors.New("wakeups: at most 32 entries")
	}
	seen := make(map[string]bool)
	for i := range entries {
		c := &entries[i]
		if !wakeupName.MatchString(c.Name) || seen[c.Name] {
			return fmt.Errorf("wakeups: invalid or duplicate name %q", c.Name)
		}
		seen[c.Name] = true
		if err := ValidateWakeupDestination(c.URL); err != nil {
			return fmt.Errorf("wakeups %s: %w", c.Name, err)
		}
		if _, _, err := c.TrustedDestination(); err != nil {
			return err
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
		if (c.PersonID != nil && *c.PersonID <= 0) || len(c.SourceIDs) > 50 || len(c.ConversationIDs) > 50 {
			return fmt.Errorf("wakeups %s: invalid selectors", c.Name)
		}
		for _, ids := range []*[]int64{&c.SourceIDs, &c.ConversationIDs} {
			for _, id := range *ids {
				if id <= 0 {
					return fmt.Errorf("wakeups %s: invalid selector", c.Name)
				}
			}
			slices.Sort(*ids)
			*ids = slices.Compact(*ids)
			if len(*ids) == 0 {
				*ids = nil
			}
		}
	}
	return nil
}

// TrustedDestination parses the configured destination pins at the config boundary.
func (c WakeupConfig) TrustedDestination() (*url.URL, []netip.Addr, error) {
	if len(c.TrustedAddresses) == 0 {
		return nil, nil, nil
	}
	target, err := url.Parse(c.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("wakeups %s: invalid callback", c.Name)
	}
	target.Path, target.RawPath, target.RawQuery = "", "", ""
	target.ForceQuery = false
	addresses := make([]netip.Addr, 0, len(c.TrustedAddresses))
	for _, raw := range c.TrustedAddresses {
		ip, err := netip.ParseAddr(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("wakeups %s: invalid trusted address", c.Name)
		}
		addresses = append(addresses, ip)
	}
	origin, addresses, err := netguard.ValidateTrustedDestination(target, addresses)
	if err != nil {
		return nil, nil, fmt.Errorf("wakeups %s: %w", c.Name, err)
	}
	return origin, addresses, nil
}

// ValidateWakeupDestination applies the shared callback rule.
func ValidateWakeupDestination(endpoint string) error {
	target, err := url.Parse(endpoint)
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || target.Fragment != "" || target.Opaque != "" || strings.Contains(endpoint, "#") || (target.Port() != "" && target.Port() != "443" && target.Port() != "8443") {
		return fmt.Errorf("invalid callback")
	}
	if ip, e := netip.ParseAddr(target.Hostname()); e == nil {
		if netguard.ProhibitedIP(ip) {
			return fmt.Errorf("prohibited callback")
		}
	} else if netguard.ProhibitedHostname(target.Hostname()) {
		return fmt.Errorf("prohibited callback")
	}
	c := WakeupConfig{URL: endpoint}
	_, _, err = c.TrustedDestination()
	return err
}
