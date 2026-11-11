package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const instagramProfile = `[[instagram_comments]]
identifier="example-account"
app_id="9"
page_id="100"
instagram_account_id="200"
token_env="EXAMPLE_INSTAGRAM_PAGE_TOKEN"
enabled=true
schedule="*/30 * * * *"
`

func TestInstagramCommentsProfiles(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := loadConfigText(t, instagramProfile)
	require.Len(cfg.InstagramComments, 1)
	assert.Equal("200", cfg.InstagramComments[0].InstagramAccountID)
	assert.Equal(cfg.InstagramComments[0], *cfg.GetInstagramCommentsSource("EXAMPLE-ACCOUNT"))
	assert.Nil(cfg.GetInstagramCommentsSource("missing"))
	assert.Len(cfg.ScheduledInstagramCommentsSources(), 1)
	for _, change := range [][2]string{{"enabled=true", "enabled=false"}, {`schedule="*/30 * * * *"`, `schedule=""`}} {
		assert.Empty(loadConfigText(t, strings.Replace(instagramProfile, change[0], change[1], 1)).ScheduledInstagramCommentsSources())
	}
	second := strings.Replace(instagramProfile, `identifier="example-account"`, `identifier="other"`, 1)
	second = strings.Replace(second, `instagram_account_id="200"`, `instagram_account_id="201"`, 1)
	assert.Len(loadConfigText(t, instagramProfile+second).InstagramComments, 2)
}

func TestInstagramCommentsRejectsInvalidProfiles(t *testing.T) {
	require := require.New(t)
	for _, change := range [][2]string{
		{`identifier="example-account"`, `identifier=" "`},
		{`app_id="9"`, `app_id="9/path"`},
		{`page_id="100"`, `page_id="100_200"`},
		{`instagram_account_id="200"`, `instagram_account_id="200/path"`},
		{`token_env="EXAMPLE_INSTAGRAM_PAGE_TOKEN"`, `token_env="TOKEN=value"`},
		{`schedule="*/30 * * * *"`, `schedule="tomorrow"`},
	} {
		require.Error(loadConfigTextError(t, strings.Replace(instagramProfile, change[0], change[1], 1)))
	}
	require.Error(loadConfigTextError(t, instagramProfile+"token=\"inline-secret\"\n"))
	for _, labels := range [][2]string{{"account", "ACCOUNT"}, {"s", "ſ"}} {
		first := strings.Replace(instagramProfile, `identifier="example-account"`, `identifier="`+labels[0]+`"`, 1)
		second := strings.Replace(instagramProfile, `identifier="example-account"`, `identifier="`+labels[1]+`"`, 1)
		second = strings.Replace(second, `instagram_account_id="200"`, `instagram_account_id="201"`, 1)
		require.ErrorContains(loadConfigTextError(t, first+second), "identifier")
	}
	second := strings.Replace(instagramProfile, `identifier="example-account"`, `identifier="other"`, 1)
	require.ErrorContains(loadConfigTextError(t, instagramProfile+second), "duplicates")
}
