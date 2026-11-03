package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstagramAdProfiles(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	cfg := loadConfigText(t, instagramProfile+"ad_account_id=\"700\"\nad_token_env=\"EXAMPLE_META_AD_TOKEN\"\n")
	source := cfg.GetInstagramCommentsSource("EXAMPLE-ACCOUNT")
	require.NotNil(source)
	assert.Equal("700", source.AdAccountID)
	assert.Equal("EXAMPLE_META_AD_TOKEN", source.AdTokenEnv)
	for _, fields := range []string{
		"ad_account_id=\"700\"\n", "ad_token_env=\"EXAMPLE_META_AD_TOKEN\"\n",
		"ad_account_id=\"act_700\"\nad_token_env=\"EXAMPLE_META_AD_TOKEN\"\n",
		"ad_account_id=\"700\"\nad_token_env=\"TOKEN=value\"\n",
		"ad_account_id=\"700\"\nad_token=\"inline-secret\"\n",
	} {
		require.Error(loadConfigTextError(t, instagramProfile+fields))
	}
}
