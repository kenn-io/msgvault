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
	assert.False((CardDAVConfig{BaseURL: "https://contacts.example.test", Username: "Synthetic@Example.net"}).SameAccount(
		CardDAVConfig{BaseURL: "https://contacts.example.test", Username: "synthetic@example.net"}))
}
