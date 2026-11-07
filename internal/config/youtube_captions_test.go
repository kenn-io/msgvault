package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestYouTubeScheduledCaptionsOptIn(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	text := `[[youtube]]
identifier="fixture"
channel_id="UCfixture"
token_env="FIXTURE_REFRESH"
client_id_env="FIXTURE_CLIENT"
client_secret_env="FIXTURE_SECRET"
enabled=true
schedule="*/30 * * * *"
captions=true
caption_units=250
`
	cfg := loadConfigText(t, text)
	scheduled := cfg.ScheduledYouTubeSources()
	requirements.Len(scheduled, 1)
	assertions.True(scheduled[0].Captions)
	assertions.Equal(250, scheduled[0].CaptionUnits)
	requirements.Error(loadConfigTextError(t, strings.Replace(text, "caption_units=250", "caption_units=-1", 1)))
}
