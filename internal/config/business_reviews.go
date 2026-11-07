package config

import "errors"

// ValidateBusinessReviews checks operational targets independently of archive
// accounts. A named app is selected later; no shared mail-token fallback exists.
func (c *Config) ValidateBusinessReviews() error {
	names := make(map[string]bool)
	for _, target := range c.BusinessReviews {
		if err := target.Validate(); err != nil {
			return errors.New("business_reviews: each target needs a unique name, Google provider, account email, explicit oauth_app, and enabled own-project location")
		}
		if names[target.Name] {
			return errors.New("business_reviews: target names must be unique")
		}
		names[target.Name] = true
	}
	return nil
}
