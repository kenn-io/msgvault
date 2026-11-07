package config

import "errors"

// PostalAddressConfig enables owner-triggered checks only. Credentials come
// exclusively from MSGVAULT_GOOGLE_ADDRESS_VALIDATION_API_KEY in the daemon.
// Enabling this never grants per-request consent or enables sweep egress.
type PostalAddressConfig struct {
	Enabled  bool   `toml:"enabled"`
	Provider string `toml:"provider"`
}

func (c PostalAddressConfig) Validate() error {
	if c.Provider != "" && c.Provider != "google" {
		return errors.New("people.address_validation.provider must be google")
	}
	if c.Enabled && c.Provider != "google" {
		return errors.New("people.address_validation.provider is required when enabled")
	}
	return nil
}
