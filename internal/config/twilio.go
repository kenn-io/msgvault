package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strings"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

// TwilioSource identifies one Twilio account or subaccount in one region.
// The daemon only reads existing calls, recordings, and retained transcripts.
type TwilioSource struct {
	Identifier             string            `toml:"identifier"`
	AccountEmail           string            `toml:"account_email"`
	AccountSID             string            `toml:"account_sid"`
	APIKeySID              string            `toml:"api_key_sid"`
	APIKeySecret           string            `toml:"api_key_secret"`
	AuthToken              string            `toml:"auth_token"`
	Region                 string            `toml:"region"`
	IntelligenceServiceSID string            `toml:"intelligence_service_sid"`
	RelayDiscovery         bool              `toml:"relay_discovery"`
	Schedule               string            `toml:"schedule"`
	Enabled                bool              `toml:"enabled"`
	Media                  *bool             `toml:"media"`
	MaxMediaMB             int               `toml:"max_media_mb"`
	RecordingKeys          map[string]string `toml:"recording_keys"`
	ExternalMedia          map[string]string `toml:"external_media"`
	ExternalMediaHosts     []string          `toml:"external_media_hosts"`
}

// EffectiveAccountEmail returns the configured primary account identity.
func (s TwilioSource) EffectiveAccountEmail() (string, error) {
	return effectiveMeetingAccountEmail("twilio", s.Identifier, s.AccountEmail)
}

// MediaPolicy caps each recording at 250 MiB unless configured otherwise.
func (s TwilioSource) MediaPolicy() attachmentpolicy.Policy {
	p := attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll, MaxBytes: 250 << 20}
	if s.MaxMediaMB > 0 && int64(s.MaxMediaMB) <= math.MaxInt64>>20 {
		p.MaxBytes = int64(s.MaxMediaMB) << 20
	}
	if s.Media != nil && !*s.Media {
		p.DisabledReason = attachmentpolicy.SkipAccountPolicy
	}
	return p
}

func (c *Config) prepareTwilioSources() error {
	if len(c.Twilio) == 1 && strings.TrimSpace(c.Twilio[0].Identifier) == "" {
		c.Twilio[0].Identifier = "default"
	}
	seen := make(map[string]bool, len(c.Twilio))
	for i := range c.Twilio {
		s := &c.Twilio[i]
		s.Identifier = strings.TrimSpace(s.Identifier)
		key := strings.ToLower(s.Identifier)
		if key == "" {
			return errors.New("[[twilio]]: every entry needs an identifier when more than one is configured")
		}
		if seen[key] {
			return fmt.Errorf("[[twilio]]: duplicate identifier %q", s.Identifier)
		}
		seen[key] = true
		email, err := s.EffectiveAccountEmail()
		if err != nil {
			return err
		}
		s.AccountEmail = email
		if s.Region == "" {
			s.Region = "us1"
		}
		if err := s.Validate(); err != nil {
			return fmt.Errorf("[[twilio]] identifier %q: %w", s.Identifier, err)
		}
		for sid, path := range s.RecordingKeys {
			s.RecordingKeys[sid] = resolveRelative(expandPath(path), c.HomeDir)
		}
	}
	return nil
}

// Validate checks credentials and explicit destinations without opening keys
// or contacting Twilio. Error messages never contain credential values or URLs.
func (s TwilioSource) Validate() error {
	if !validTwilioSID(s.AccountSID, "AC") {
		return errors.New("account_sid must be an AC SID")
	}
	key := strings.TrimSpace(s.APIKeySID) != ""
	secret := strings.TrimSpace(s.APIKeySecret) != ""
	token := strings.TrimSpace(s.AuthToken) != ""
	if key != secret {
		return errors.New("api_key_sid and api_key_secret must be configured together")
	}
	if key && token {
		return errors.New("API key credentials and auth_token are mutually exclusive")
	}
	if !key && !token {
		return errors.New("configure api_key_sid and api_key_secret, or auth_token")
	}
	if key && !validTwilioSID(s.APIKeySID, "SK") {
		return errors.New("api_key_sid must be an SK SID")
	}
	switch s.Region {
	case "", "us1", "ie1", "au1":
	default:
		return errors.New("region must be us1, ie1, or au1")
	}
	if s.IntelligenceServiceSID != "" && !validTwilioSID(s.IntelligenceServiceSID, "GA") {
		return errors.New("intelligence_service_sid must be a GA SID")
	}
	if s.MaxMediaMB < 0 || int64(s.MaxMediaMB) > math.MaxInt64>>20 {
		return errors.New("max_media_mb must be zero or a positive representable MiB limit")
	}
	for sid, path := range s.RecordingKeys {
		if !validTwilioSID(sid, "CR") {
			return errors.New("recording_keys requires CR public key SIDs")
		}
		if strings.TrimSpace(path) == "" || strings.Contains(path, "://") || strings.ContainsAny(path, "\x00\r\n") {
			return errors.New("recording_keys values must be local PEM file paths")
		}
	}
	hosts := make(map[string]bool, len(s.ExternalMediaHosts))
	for _, host := range s.ExternalMediaHosts {
		if !validTwilioExternalHost(host) {
			return errors.New("external_media_hosts must contain public DNS hostnames without schemes, ports, or paths")
		}
		hosts[strings.ToLower(host)] = true
	}
	for sid, address := range s.ExternalMedia {
		if !validTwilioSID(sid, "RE") {
			return errors.New("external_media requires RE recording SIDs")
		}
		u, err := url.Parse(address)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || !validTwilioExternalHost(u.Hostname()) || (u.Port() != "" && u.Port() != "443") {
			return errors.New("external_media URLs must use HTTPS on a public DNS hostname without embedded credentials")
		}
		if !hosts[strings.ToLower(u.Hostname())] {
			return errors.New("external_media URL host must be explicitly listed in external_media_hosts")
		}
	}
	return nil
}

func validTwilioSID(sid, prefix string) bool {
	if len(sid) != 34 || !strings.HasPrefix(sid, prefix) {
		return false
	}
	return strings.Trim(sid[2:], "0123456789abcdefABCDEF") == ""
}

func validTwilioExternalHost(host string) bool {
	host = strings.ToLower(host)
	if len(host) > 253 || !strings.Contains(host, ".") || net.ParseIP(host) != nil || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return false
		}
	}
	return true
}

// GetTwilioSource matches a configured source label without case sensitivity.
func (c *Config) GetTwilioSource(identifier string) *TwilioSource {
	for _, s := range c.Twilio {
		if strings.EqualFold(s.Identifier, identifier) {
			return &s
		}
	}
	return nil
}

// ScheduledTwilioSources returns enabled sources with a daemon cron schedule.
func (c *Config) ScheduledTwilioSources() []TwilioSource {
	var out []TwilioSource
	for _, s := range c.Twilio {
		if s.Enabled && s.Schedule != "" {
			out = append(out, s)
		}
	}
	return out
}
