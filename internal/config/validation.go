package config

import (
	"errors"
	"fmt"
	"slices"
)

// Section identifies a group of semantic configuration checks. TOML decoding
// and the existing unknown-key checks always apply to the entire file.
type Section string

// SectionAll requests full validation, including server-only sections.
const SectionAll Section = "*"

// LoadForSections loads all values and runtime overrides, then validates only
// the sections the caller uses. Call UnusedSectionErrors to report the rest.
// Load and the snapshot loaders retain full validation for daemon reloads.
func LoadForSections(path, home string, overrides RuntimeOverrides, sections ...Section) (*Config, error) {
	cfg, err := loadWithOverrides(path, home, overrides)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateFor(sections...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadConfigFileForSections validates selected sections in captured TOML bytes.
// Like LoadConfigFile, it does not apply process environment overrides.
func LoadConfigFileForSections(snapshot ConfigFile, home string, sections ...Section) (*Config, error) {
	cfg, err := decodeConfigFile(snapshot, home, nil)
	if err != nil {
		return nil, err
	}
	if err := cfg.ValidateFor(sections...); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) addSectionError(section Section, err error) {
	if c.sectionErrors == nil {
		c.sectionErrors = make(map[Section]error)
	}
	c.sectionErrors[section] = errors.Join(c.sectionErrors[section], err)
}

type sectionValidator struct {
	section  Section
	validate func() error
}

func (c *Config) sectionValidators() []sectionValidator {
	validators := []sectionValidator{
		{"fastmail", func() error { return c.validateFastmailSources(fastmailSourceIDConfigured(c.configContent)) }},
		{"imap.drafts", func() error { return (&Config{IMAP: c.IMAP}).validateDraftSources(c.configContent) }},
		{"gmail.drafts", func() error { return (&Config{Gmail: c.Gmail}).validateDraftSources(c.configContent) }},
		{"beeper.drafts", func() error { return (&Config{Beeper: c.Beeper}).validateDraftSources(c.configContent) }},
		{"attachments.documents", func() error {
			if err := c.Attachments.Documents.Validate(); err != nil {
				return err
			}
			if c.Attachments.Documents.Index.Embeddings.Enabled && !c.Vector.Enabled {
				return errors.New("attachments.documents.index.embeddings.enabled: requires [vector] enabled = true")
			}
			return nil
		}},
		{"vector", func() error {
			if c.Vector.AnyLaneEnabled() {
				if err := c.Vector.Validate(); err != nil {
					return fmt.Errorf("vector config: %w", err)
				}
			}
			return nil
		}},
		{"oauth", func() error {
			if err := c.OAuth.Validate(); err != nil {
				return fmt.Errorf("oauth: %w", err)
			}
			return nil
		}},
		{"server", c.Server.Validate},
		{"remote", func() error { return nil }}, // Runtime boolean overrides can fail before client credentials are needed.
		{"analytics", c.Analytics.Validate},
		{"web", c.Web.Validate},
		{"carddav", func() error {
			if !ValidCardDAVProvider(c.CardDAV.Provider) {
				return errors.New("carddav.provider must be empty, \"google\" or \"microsoft\"")
			}
			_, _, err := c.CardDAV.TrustedDestination()
			return err
		}},
		{"carddav_connections", c.validateCardDAVConnections},
		{"integrations.tasks", func() error { return c.Integrations.Tasks.Validate("tasks") }},
		{"integrations.kata", func() error { return c.Integrations.Kata.Validate("kata") }},
		{"integrations.docbank", c.Integrations.Docbank.validate},
		{"activity", c.Activity.Validate},
		{"people.sweep", c.People.Sweep.Validate},
		{"people.enrichment", c.People.Enrichment.Validate},
		{"people.identity_scoring", c.People.IdentityScoring.Validate},
		{"backup", c.Backup.Validate},
		{"chatwoot", c.validateChatwootSources},
	}
	for _, section := range []string{"beeper", "slack", "discord", "teams"} {
		validators = append(validators, sectionValidator{Section(section), func() error { return c.validateMediaPolicies(section) }})
	}
	// These copies share the original source slices, so element normalization
	// still updates c while validation stays isolated to one provider.
	for _, source := range []struct {
		section Section
		cfg     Config
	}{
		{"twenty", Config{Twenty: c.Twenty}}, {"omi", Config{Omi: c.Omi}},
		{"granola", Config{Granola: c.Granola}}, {"circleback", Config{Circleback: c.Circleback}},
		{"plaud", Config{Plaud: c.Plaud}}, {"notion_meetings", Config{NotionMeetings: c.NotionMeetings}},
		{"muesli", Config{Muesli: c.Muesli}}, {"twilio", Config{Twilio: c.Twilio}},
	} {
		validators = append(validators, sectionValidator{source.section, source.cfg.validateMeetingSources})
	}
	return validators
}

// ValidateFor checks the named sections. SectionAll validates the whole file.
func (c *Config) ValidateFor(sections ...Section) error {
	validators := c.sectionValidators()
	for _, section := range sections {
		if section == SectionAll {
			continue
		}
		if !slices.ContainsFunc(validators, func(v sectionValidator) bool { return v.section == section }) {
			return fmt.Errorf("unknown config validation section %q", section)
		}
	}
	for _, v := range validators {
		if slices.Contains(sections, SectionAll) || slices.Contains(sections, v.section) {
			if err := c.validateSection(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Config) validateSection(v sectionValidator) error {
	return errors.Join(c.sectionErrors[v.section], v.validate())
}

// UnusedSectionErrors reports invalid unused sections without replacing their
// values. CLI callers print these on stderr so ignored errors remain visible.
func (c *Config) UnusedSectionErrors(sections ...Section) []error {
	if slices.Contains(sections, SectionAll) {
		return nil
	}
	var warnings []error
	for _, v := range c.sectionValidators() {
		if !slices.Contains(sections, v.section) {
			if err := c.validateSection(v); err != nil {
				warnings = append(warnings, fmt.Errorf("[%s]: %w", v.section, err))
			}
		}
	}
	return warnings
}
