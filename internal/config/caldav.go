package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type CalDAVConfig struct {
	Sources []CalDAVSource `toml:"sources"`
}

// CalDAVSource keeps credentials in a separate file on the daemon host.
type CalDAVSource struct {
	Name             string   `toml:"name"`
	URL              string   `toml:"url"`
	Username         string   `toml:"username"`
	PasswordFile     string   `toml:"password_file"`
	TimeZone         string   `toml:"time_zone"`
	Calendars        []string `toml:"calendars"`
	TrustedOrigin    string   `toml:"trusted_origin"`
	TrustedAddresses []string `toml:"trusted_addresses"`
}

func (c *Config) GetCalDAVSource(name string) *CalDAVSource {
	for i := range c.CalDAV.Sources {
		if c.CalDAV.Sources[i].Name == name {
			return &c.CalDAV.Sources[i]
		}
	}
	return nil
}
func (c *Config) validateCalDAV() error {
	names := map[string]bool{}
	for i := range c.CalDAV.Sources {
		s := &c.CalDAV.Sources[i]
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
		if s.TimeZone == "" {
			s.TimeZone = "UTC"
		}
		if s.TimeZone == "Local" {
			return errors.New("caldav.time_zone cannot be Local")
		}
		if _, err := time.LoadLocation(s.TimeZone); err != nil {
			return fmt.Errorf("caldav.time_zone: %w", err)
		}
		s.PasswordFile = c.credentialPath(s.PasswordFile)
		policy := CardDAVConfig{TrustedOrigin: s.TrustedOrigin, TrustedAddresses: s.TrustedAddresses}
		if _, _, err := policy.TrustedDestination(); err != nil {
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
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		return "443"
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}
