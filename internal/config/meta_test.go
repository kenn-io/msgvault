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
	t.Run("scheduled profiles and distinct selection", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		cfg := loadConfigText(t, facebookProfile)
		require.Len(cfg.FacebookComments, 1)
		assert.Equal("100", cfg.FacebookComments[0].PageID)
		assert.Equal(cfg.FacebookComments[0], *cfg.GetFacebookCommentsSource("EXAMPLE-PAGE"))
		assert.Nil(cfg.GetFacebookCommentsSource("missing"))
		assert.Len(cfg.ScheduledFacebookCommentsSources(), 1)
		other := strings.Replace(facebookProfile, `identifier="example-page"`, `identifier="other-page"`, 1)
		other = strings.Replace(other, `page_id="100"`, `page_id="101"`, 1)
		cfg = loadConfigText(t, facebookProfile+other)
		require.Len(cfg.FacebookComments, 2)
		first, second := cfg.GetFacebookCommentsSource("EXAMPLE-PAGE"), cfg.GetFacebookCommentsSource("OTHER-PAGE")
		require.NotNil(first)
		require.NotNil(second)
		assert.Equal("100", first.PageID)
		assert.Equal("101", second.PageID)
	})
	for _, excluded := range []struct{ name, old, replacement string }{
		{"disabled", "enabled=true", "enabled=false"},
		{"no schedule", `schedule="*/30 * * * *"`, `schedule=""`},
	} {
		t.Run(excluded.name, func(t *testing.T) {
			assert := assert.New(t)
			cfg := loadConfigText(t, strings.Replace(facebookProfile, excluded.old, excluded.replacement, 1))
			assert.Empty(cfg.ScheduledFacebookCommentsSources())
		})
	}
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
	for _, labels := range [][2]string{{"example-page", "EXAMPLE-PAGE"}, {"s", "\u017f"}, {"\u017f", "s"}} {
		t.Run(labels[0]+"/"+labels[1], func(t *testing.T) {
			first := strings.Replace(facebookProfile, `identifier="example-page"`, `identifier="`+labels[0]+`"`, 1)
			second := strings.Replace(facebookProfile, `identifier="example-page"`, `identifier="`+labels[1]+`"`, 1)
			second = strings.Replace(second, `page_id="100"`, `page_id="101"`, 1)
			require.ErrorContains(t, loadConfigTextError(t, first+second), "identifier")
		})
	}
}

func TestFacebookCommentsActionsValidate(t *testing.T) {
	for _, tc := range []struct {
		actions string
		bad     bool
	}{
		{`[]`, false}, {`["hide","delete","public_reply","private_reply"]`, false},
		{`["hide","hide"]`, true}, {`["send"]`, true}, {`["Hide"]`, true},
	} {
		t.Run(tc.actions, func(t *testing.T) {
			err := loadConfigTextError(t, facebookProfile+"actions="+tc.actions+"\n")
			if tc.bad {
				require.ErrorContains(t, err, "facebook_comments[0].actions has unknown or duplicate action")
			} else {
				require.NoError(t, err)
			}
		})
	}
	cfg := loadConfigText(t, facebookProfile+"actions=[\"hide\"]\n")
	assert.Equal(t, []string{"hide"}, cfg.FacebookComments[0].Actions)
}
func TestFacebookCommentsActionAllowedUnhideMapsToHide(t *testing.T) {
	p := FacebookCommentsSource{Enabled: true, Actions: []string{"hide"}}
	assert.True(t, p.ActionAllowed("unhide"))
	assert.False(t, p.ActionAllowed("delete"))
	p.Enabled = false
	assert.False(t, p.ActionAllowed("hide"))
}
func TestFacebookCommentsSourceForPair(t *testing.T) {
	cfg := loadConfigText(t, facebookProfile)
	require.NotNil(t, cfg.FacebookCommentsSourceFor("9", "100"))
	assert.Nil(t, cfg.FacebookCommentsSourceFor("99", "100"))
	assert.Nil(t, cfg.FacebookCommentsSourceFor("9", "101"))
}
