package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetaInboxProfiles(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	facebook := "[[facebook_messages]]\nidentifier=\"example-page\"\napp_id=\"9\"\npage_id=\"100\"\ntoken_env=\"EXAMPLE_PAGE_TOKEN\"\n"
	instagram := strings.ReplaceAll(instagramProfile, "instagram_comments", "instagram_messages")
	cfg := loadConfigText(t, facebook+instagram)
	require.Len(cfg.FacebookMessages, 1)
	require.Len(cfg.InstagramMessages, 1)
	assert.Equal("200", cfg.InstagramMessages[0].InstagramAccountID)

	require.ErrorContains(loadConfigTextError(t, facebook+facebook), "facebook_messages[1]")
	require.ErrorContains(loadConfigTextError(t, instagram+"mentions=true\n"), "mentions")
	require.ErrorContains(loadConfigTextError(t, strings.Replace(facebook, `page_id="100"`, `page_id="x"`, 1)), "facebook_messages[0]")
}
