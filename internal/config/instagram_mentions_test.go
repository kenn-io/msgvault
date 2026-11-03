package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInstagramMentionsKeyParses(t *testing.T) {
	cfg := loadConfigText(t, instagramProfile+"mentions=true\n")
	cfg.Data.DataDir = t.TempDir()
	assert.True(t, cfg.InstagramComments[0].Mentions)
	assert.False(t, loadConfigText(t, instagramProfile).InstagramComments[0].Mentions)
}

func TestScheduledInstagramMentionsSources(t *testing.T) {
	for _, change := range [][2]string{{"mentions=true", "mentions=false"}, {"enabled=true", "enabled=false"}, {`schedule="*/30 * * * *"`, `schedule=""`}} {
		cfg := loadConfigText(t, instagramProfile+"mentions=true\n")
		cfg.Data.DataDir = t.TempDir()
		assert.Len(t, cfg.ScheduledInstagramMentionsSources(), 1)
		cfg = loadConfigText(t, strings.Replace(instagramProfile+"mentions=true\n", change[0], change[1], 1))
		assert.Empty(t, cfg.ScheduledInstagramMentionsSources())
	}
}
