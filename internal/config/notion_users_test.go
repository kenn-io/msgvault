package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadNotionUsersToken(t *testing.T) {
	path := writeMeetingConfig(t, `[[notion_meetings]]
account_email = "owner@example.com"
token = "pat-example"
users_token = "ntn-example"
schedule = "15 */6 * * *"
enabled = true
`)
	cfg, err := Load(path, "")
	require.NoError(t, err)
	require.Len(t, cfg.NotionMeetings, 1)
	assert.Equal(t, "ntn-example", cfg.NotionMeetings[0].UsersToken)
	assert.Equal(t, "pat-example", cfg.NotionMeetings[0].Token)
	assert.True(t, cfg.NotionMeetings[0].Enabled)
}
