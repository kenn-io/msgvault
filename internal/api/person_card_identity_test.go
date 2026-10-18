package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestPersonCardConfiguredOwnerIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	cfg := &config.Config{Granola: []config.GranolaSource{{Identifier: "meeting-account", AccountEmail: "Owner@Example.com"}}, Accounts: []config.AccountSchedule{{Email: "mail@example.com"}}}
	got, err := personCardPrimaryEmail(cfg, "granola", "meeting-account")
	requirements.NoError(err)
	assertions.Equal("owner@example.com", got)
	got, err = personCardPrimaryEmail(cfg, "gmail", "mail@example.com")
	requirements.NoError(err)
	assertions.Equal("mail@example.com", got)
	got, err = personCardPrimaryEmail(cfg, "granola", "other")
	requirements.NoError(err)
	assertions.Empty(got)
}
