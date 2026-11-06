package config

import "errors"

// EmailPushConfig opts exact stored email sources into provider hints. Empty
// lists disable listeners. Every selected source still needs an enabled account
// polling schedule, checked against the archive when the daemon starts.
type EmailPushConfig struct {
	IMAPSourceIDs []int64 `toml:"imap_source_ids"`
}

func (c EmailPushConfig) Validate() error {
	if len(c.IMAPSourceIDs) > 8 {
		return errors.New("email_push.imap_source_ids supports at most eight sources")
	}
	seen := make(map[int64]bool, len(c.IMAPSourceIDs))
	for _, id := range c.IMAPSourceIDs {
		if id <= 0 || seen[id] {
			return errors.New("email_push.imap_source_ids requires unique positive source IDs")
		}
		seen[id] = true
	}
	return nil
}
