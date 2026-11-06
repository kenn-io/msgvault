package config

import (
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func validGmailPushConfig() GmailPushConfig {
	return GmailPushConfig{SourceID: 1, Topic: "projects/example/topics/mail", Subscription: "projects/example/subscriptions/mail", CredentialsFile: "/synthetic/subscriber.json"}
}

func TestGmailPushConfigAcceptanceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		sourceID := hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		suffix := hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))
		c := GmailPushConfig{SourceID: sourceID, Topic: fmt.Sprintf("projects/example/topics/mail-%d", suffix), Subscription: fmt.Sprintf("projects/other/subscriptions/mail-%d", suffix), CredentialsFile: fmt.Sprintf("/synthetic/subscriber-%d.json", suffix)}
		require.NoError(ht, c.Validate())
		require.NoError(ht, ValidateGmailPushConfigs([]GmailPushConfig{c}))
	})
}

func TestGmailPushConfigRejectsIncomplete(t *testing.T) {
	for _, field := range []string{"source", "topic", "subscription", "credentials"} {
		t.Run(field, func(t *testing.T) {
			c := validGmailPushConfig()
			switch field {
			case "source":
				c.SourceID = 0
			case "topic":
				c.Topic = ""
			case "subscription":
				c.Subscription = ""
			case "credentials":
				c.CredentialsFile = " "
			}
			require.Error(t, c.Validate())
			require.Error(t, ValidateGmailPushConfigs([]GmailPushConfig{c}))
		})
	}
}

func TestGmailPushConfigRejectsNegativeIDProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		c := validGmailPushConfig()
		c.SourceID = hegel.Draw(ht, hegel.Integers(int64(math.MinInt64), int64(0)))
		require.Error(ht, c.Validate())
	})
}

func TestGmailPushConfigRejectsDuplicateSourceProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		c := validGmailPushConfig()
		c.SourceID = hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64)))
		other := c
		other.Subscription = "projects/example/subscriptions/other"
		require.Error(ht, ValidateGmailPushConfigs([]GmailPushConfig{c, other}))
	})
}

func TestGmailPushConfigRejectsSharedSubscriptionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		c := validGmailPushConfig()
		c.SourceID = hegel.Draw(ht, hegel.Integers(int64(1), int64(math.MaxInt64-1)))
		other := c
		other.SourceID = c.SourceID + 1
		require.Error(ht, ValidateGmailPushConfigs([]GmailPushConfig{c, other}))
	})
}

func TestGmailPushConfigAllowsSeparateSubscriptions(t *testing.T) {
	first := validGmailPushConfig()
	second := first
	second.SourceID = 2
	second.Subscription = "projects/example/subscriptions/other"
	require.NoError(t, ValidateGmailPushConfigs([]GmailPushConfig{first, second}))
	require.NoError(t, ValidateGmailPushConfigs(nil))
}

func TestGmailPushConfigResourceInjectionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		suffix := hegel.Draw(ht, hegel.Text().MaxSize(60))
		c := validGmailPushConfig()
		c.Subscription += "?" + suffix
		err := c.Validate()
		require.Error(ht, err)
		assert.NotContains(ht, err.Error(), c.Subscription)
	})
}

func TestEmailPushGmailConfigurationWiring(t *testing.T) {
	requirements := require.New(t)
	cfg := NewDefaultConfig()
	requirements.Empty(cfg.EmailPush.Gmail, "Gmail push defaults off")
	cfg.EmailPush.Gmail = []GmailPushConfig{validGmailPushConfig()}
	requirements.NoError(cfg.EmailPush.Validate())
	cfg.EmailPush.Gmail = append(cfg.EmailPush.Gmail, cfg.EmailPush.Gmail[0])
	cfg.EmailPush.Gmail[1].SourceID = 2
	requirements.Error(cfg.EmailPush.Validate(), "shared subscriptions across sources fail before startup")
}
