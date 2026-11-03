package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDMSourceIdentifiers(t *testing.T) {
	assert := assert.New(t)

	for _, src := range []DMSource{{AppID: "9", PageID: "100", Platform: PlatformMessenger}, {AppID: "9", PageID: "100", IGID: "1784", Platform: PlatformInstagram}} {
		got, ok := ParseDMSourceIdentifier(src.Platform, src.Identifier())
		assert.True(ok)
		assert.Equal(src, got)
		for _, bad := range []string{"app:bad:page:100", src.Identifier() + ":extra", "app:9:page:100/path", "app:9:page:0", "app:9:page:100:instagram:no"} {
			_, ok = ParseDMSourceIdentifier(src.Platform, bad)
			assert.False(ok, bad)
		}
	}
	assert.Equal(SourceTypeFacebookMessages, PlatformMessenger.SourceType())
	assert.Equal(SourceTypeInstagramMessages, PlatformInstagram.SourceType())
	assert.Equal("messenger", PlatformMessenger.GraphPlatform())
	assert.Equal("instagram", PlatformInstagram.GraphPlatform())
	_, ok := ParseDMSourceIdentifier(DMPlatform("unknown"), "app:9:page:100")
	assert.False(ok)
}
