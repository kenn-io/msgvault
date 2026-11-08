package config

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNotionDiscoveryConfigurationNormalizesScope(t *testing.T) {
	assert := assert.New(t)

	path := writeMeetingConfig(t, `[[notion_meetings]]
identifier="work"
account_email="owner@example.com"
token="synthetic"
token_type=" PAT "
user_id=" human-1 "
root_page_ids=[" page-1 ","page-1","page-2"]
root_data_source_ids=[" data-1 ","data-1"]
meeting_block_ids=[" meeting-1 ","meeting-1"]
search_accessible=false
`)
	cfg, err := Load(path, "")
	require.NoError(t, err)
	source := cfg.NotionMeetings[0]
	assert.Equal("pat", source.TokenType)
	assert.Equal("human-1", source.UserID)
	assert.Equal([]string{"page-1", "page-2"}, source.RootPageIDs)
	assert.Equal([]string{"data-1"}, source.RootDataSourceIDs)
	assert.Equal([]string{"meeting-1"}, source.MeetingBlockIDs)
	assert.False(source.SearchAccessibleEnabled())
	assert.True((NotionMeetingsSource{}).SearchAccessibleEnabled())
}
func TestNotionDiscoveryConfigurationRejectsInvalidScope(t *testing.T) {
	for _, setting := range []string{`token_type="oauth-guessed"`, `root_page_ids=[" "]`, `root_data_source_ids=[""]`, `meeting_block_ids=["ok",""]`} {
		t.Run(setting, func(t *testing.T) {
			path := writeMeetingConfig(t, "[[notion_meetings]]\nidentifier=\"work\"\naccount_email=\"owner@example.com\"\n"+setting+"\n")
			_, err := Load(path, "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "notion_meetings")
		})
	}
}
