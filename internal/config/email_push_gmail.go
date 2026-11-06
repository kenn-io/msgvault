package config

import (
	"errors"
	"regexp"
	"strings"
)

// GmailPushConfig assigns an exclusive ordinary Pull subscription to one stored
// Gmail source. The source must retain an enabled canonical polling schedule.
// CredentialsFile is an explicit subscriber JSON path, separate from Gmail OAuth.
type GmailPushConfig struct {
	SourceID        int64  `toml:"source_id"`
	Topic           string `toml:"topic"`
	Subscription    string `toml:"subscription"`
	CredentialsFile string `toml:"credentials_file"`
}

var (
	gmailPushTopicPattern        = regexp.MustCompile(`^projects/[A-Za-z0-9][A-Za-z0-9.:-]{0,127}/topics/[A-Za-z][A-Za-z0-9._~+-]{0,254}$`)
	gmailPushSubscriptionPattern = regexp.MustCompile(`^projects/[A-Za-z0-9][A-Za-z0-9.:-]{0,127}/subscriptions/[A-Za-z][A-Za-z0-9._~+-]{0,254}$`)
)

// Validate checks static configuration without reading credentials or providers.
// Provider mode/topic validation and source/profile binding occur before listening.
func (c GmailPushConfig) Validate() error {
	if c.SourceID <= 0 {
		return errors.New("email_push.gmail requires a positive source_id")
	}
	if !gmailPushTopicPattern.MatchString(c.Topic) {
		return errors.New("email_push.gmail requires a full topic resource")
	}
	if !gmailPushSubscriptionPattern.MatchString(c.Subscription) {
		return errors.New("email_push.gmail requires a full subscription resource")
	}
	if strings.TrimSpace(c.CredentialsFile) == "" {
		return errors.New("email_push.gmail requires an explicit credentials_file")
	}
	return nil
}

// ValidateGmailPushConfigs rejects duplicate sources and subscriptions. Different
// subscriptions may use the same topic; no other consumer may share a configured
// subscription. An empty list disables Gmail push.
func ValidateGmailPushConfigs(entries []GmailPushConfig) error {
	sources := make(map[int64]bool, len(entries))
	subscriptions := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if err := entry.Validate(); err != nil {
			return err
		}
		if sources[entry.SourceID] {
			return errors.New("email_push.gmail requires unique source IDs")
		}
		if subscriptions[entry.Subscription] {
			return errors.New("email_push.gmail requires one exclusive subscription per source")
		}
		sources[entry.SourceID] = true
		subscriptions[entry.Subscription] = true
	}
	return nil
}
