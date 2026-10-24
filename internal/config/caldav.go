package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/netguard"
)

const DefaultCalDAVMaxEvents = 20000

type CalDAVConfig struct {
	Sources []CalDAVSource `toml:"sources"`
}

// CalDAVSource keeps credentials in a separate file on the daemon host.
type CalDAVSource struct {
	Name             string   `toml:"name"`
	URL              string   `toml:"url"`
	Username         string   `toml:"username"`
	PasswordFile     string   `toml:"password_file"`
	TimeZone         string   `toml:"time_zone,omitempty"`
	MaxEvents        int      `toml:"max_events,omitzero"`
	Calendars        []string `toml:"calendars"`
	TrustedOrigin    string   `toml:"trusted_origin"`
	TrustedAddresses []string `toml:"trusted_addresses"`
}

func (s CalDAVSource) TrustedDestination() (*url.URL, []netip.Addr, error) {
	return (CardDAVConfig{TrustedOrigin: s.TrustedOrigin, TrustedAddresses: s.TrustedAddresses}).TrustedDestination()
}

// GetCalDAVSource returns a copy with runtime defaults and a resolved password path.
func (c *Config) GetCalDAVSource(name string) *CalDAVSource {
	for _, source := range c.CalDAV.Sources {
		if source.Name == name {
			if source.MaxEvents == 0 {
				source.MaxEvents = DefaultCalDAVMaxEvents
			}
			if source.TimeZone == "" {
				source.TimeZone = "UTC"
			}
			source.PasswordFile = c.credentialPath(source.PasswordFile)
			return &source
		}
	}
	return nil
}
func (c *Config) validateCalDAV() error {
	names := map[string]bool{}
	for _, s := range c.CalDAV.Sources {
		if err := ValidateCardDAVConnectionName(s.Name); err != nil {
			return fmt.Errorf("caldav source name: %w", err)
		}
		if names[s.Name] {
			return errors.New("duplicate caldav source name")
		}
		names[s.Name] = true
		u, err := url.Parse(s.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return errors.New("caldav.url requires an HTTPS URL without credentials, query or fragment")
		}
		if s.Username == "" || s.PasswordFile == "" {
			return errors.New("caldav source requires username and password_file")
		}
		if s.MaxEvents < 0 || s.MaxEvents > 200000 {
			return errors.New("caldav.max_events must be between 1 and 200000")
		}
		if s.TimeZone == "Local" {
			return errors.New("caldav.time_zone cannot be Local")
		}
		if _, err := time.LoadLocation(s.TimeZone); err != nil {
			return fmt.Errorf("caldav.time_zone: %w", err)
		}
		if _, _, err := s.TrustedDestination(); err != nil {
			return fmt.Errorf("caldav trusted destination: %w", err)
		}
		for _, raw := range s.Calendars {
			collection, err := url.Parse(raw)
			if err != nil || collection.Scheme != u.Scheme || !sameCalDAVOrigin(collection, u) || collection.User != nil || collection.RawQuery != "" || collection.Fragment != "" || !strings.HasSuffix(collection.Path, "/") {
				return errors.New("caldav.calendars requires absolute same-origin collection URLs ending with slash")
			}
		}
	}
	return nil
}

func sameCalDAVOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Hostname(), b.Hostname()) && netguard.EffectivePort(a) == netguard.EffectivePort(b)
}
