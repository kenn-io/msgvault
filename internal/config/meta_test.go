package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const facebookProfile = `[[facebook_comments]]
identifier="example-page"
app_id="9"
page_id="100"
token_env="EXAMPLE_FACEBOOK_PAGE_TOKEN"
enabled=true
schedule="*/30 * * * *"
`

func TestFacebookCommentsProfiles(t *testing.T) {
	cfg := loadConfigText(t, facebookProfile)
	require.Len(t, cfg.FacebookComments, 1)
	assert.Equal(t, "100", cfg.FacebookComments[0].PageID)
	assert.Equal(t, cfg.FacebookComments[0], *cfg.GetFacebookCommentsSource("EXAMPLE-PAGE"))
	assert.Nil(t, cfg.GetFacebookCommentsSource("missing"))
	assert.Len(t, cfg.ScheduledFacebookCommentsSources(), 1)
}

func TestFacebookCommentsRejectsInvalidProfiles(t *testing.T) {
	for _, tt := range []struct{ old, new string }{
		{`identifier="example-page"`, `identifier=""`},
		{`app_id="9"`, `app_id="9/path"`},
		{`page_id="100"`, `page_id="100_200"`},
		{`token_env="EXAMPLE_FACEBOOK_PAGE_TOKEN"`, `token_env=""`},
		{`token_env="EXAMPLE_FACEBOOK_PAGE_TOKEN"`, `token_env="TOKEN=value"`},
		{`schedule="*/30 * * * *"`, `schedule="tomorrow"`},
	} {
		t.Run(tt.new, func(t *testing.T) {
			err := loadConfigTextError(t, strings.Replace(facebookProfile, tt.old, tt.new, 1))
			require.Error(t, err)
		})
	}
	require.Error(t, loadConfigTextError(t, facebookProfile+"token=\"never-accept-inline-token\"\n"))
	for _, profile := range []string{
		strings.Replace(facebookProfile, `identifier="example-page"`, `identifier="other"`, 1),
		strings.Replace(facebookProfile, `page_id="100"`, `page_id="101"`, 1),
	} {
		require.Error(t, loadConfigTextError(t, facebookProfile+profile))
	}
}
