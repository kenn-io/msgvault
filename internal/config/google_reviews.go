package config

import "errors"

// ValidateGoogleReviews checks operational targets independently of archive
// accounts. A named app is selected later; no shared mail-token fallback exists.
func (c *Config) ValidateGoogleReviews() error {
	names := make(map[string]bool)
	for _, target := range c.GoogleReviews {
		if err := target.Validate(); err != nil {
			return errors.New("google_reviews: each target needs a unique name, Google provider, account email, explicit oauth_app, and enabled own-project location")
		}
		if names[target.Name] {
			return errors.New("google_reviews: target names must be unique")
		}
		names[target.Name] = true
	}
	return nil
}
