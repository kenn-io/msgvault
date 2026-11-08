package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestICloudConnectionsRejectCaseOnlyDuplicate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	first := CardDAVConfig{BaseURL: "https://p42-contacts.icloud.com/", Username: "Synthetic@Example.net"}
	next := CardDAVConfig{BaseURL: "https://p42-contacts.icloud.com", Username: "synthetic@example.net"}
	cfg := &Config{CardDAV: first}
	require.ErrorIs(cfg.ValidateCardDAVConnectionIdentity("other", next), ErrDuplicateCardDAVAccount)
	next.BaseURL = "https://p43-contacts.icloud.com:443/"
	require.ErrorIs(cfg.ValidateCardDAVConnectionIdentity("other", next), ErrDuplicateCardDAVAccount)
	next.Username = "different@example.net"
	assert.NoError(cfg.ValidateCardDAVConnectionIdentity("other", next))
	assert.False((CardDAVConfig{BaseURL: "https://contacts.example.test", Username: "Synthetic@Example.net"}).SameAccount(
		CardDAVConfig{BaseURL: "https://contacts.example.test", Username: "synthetic@example.net"}))
	for _, baseURL := range []string{"https://p42-contacts.icloud.com.example.test", "https://p42-contacts.icloud.com/private/"} {
		assert.False((CardDAVConfig{BaseURL: baseURL, Username: "Synthetic@Example.net"}).SameAccount(
			CardDAVConfig{BaseURL: baseURL, Username: "synthetic@example.net"}))
	}
}
