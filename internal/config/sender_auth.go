package config

import (
	"errors"
	"go.kenn.io/msgvault/internal/senderauth"
)

// SenderAuthConfig pins receipt headers to source IDs in this archive. Pins
// require trusted original MIME; matching a hostname alone proves no provenance.
type SenderAuthConfig struct {
	Sources []SenderAuthSource `toml:"sources"`
}
type SenderAuthSource struct {
	SourceID           int64    `toml:"source_id"`
	TrustedAuthservIDs []string `toml:"trusted_authserv_ids"`
}

func (c SenderAuthConfig) Validate() error {
	seen := map[int64]bool{}
	for _, source := range c.Sources {
		if source.SourceID <= 0 || seen[source.SourceID] {
			return errors.New("sender_auth: source_id must be positive and unique")
		}
		seen[source.SourceID] = true
		for _, pin := range source.TrustedAuthservIDs {
			if !senderauth.ValidTrustPattern(pin) {
				return errors.New("sender_auth: invalid trusted_authserv_ids pattern")
			}
		}
	}
	return nil
}
func (c SenderAuthConfig) Pins() map[int64]senderauth.TrustSet {
	pins := make(map[int64]senderauth.TrustSet, len(c.Sources))
	for _, source := range c.Sources {
		pins[source.SourceID] = append(senderauth.TrustSet(nil), source.TrustedAuthservIDs...)
	}
	return pins
}
