package discord

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestPublicChannelCatalogExcludesPrivateThreadsAndOtherParents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	peer := newImporterFakeAPI(
		Channel{ID: "301", Type: channelTypeGuildText},
		Channel{ID: "302", Type: channelTypeGuildText},
	)
	peer.active = []Channel{
		{ID: "401", ParentID: "301", Type: channelTypePublicThread},
		{ID: "402", ParentID: "301", Type: channelTypePrivateThread},
		{ID: "403", ParentID: "302", Type: channelTypePublicThread},
	}
	var queriedParents []string
	peer.archiveHook = func(parent string, private bool, _ ArchiveCursor) (ThreadPage, error) {
		assert.False(private, "public collection must never request private archives")
		queriedParents = append(queriedParents, parent)
		return ThreadPage{Threads: []Channel{
			{ID: "404", ParentID: parent, Type: channelTypePublicThread, ThreadMetadata: &ThreadMetadata{Archived: true, ArchiveTimestamp: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)}},
			{ID: "405", ParentID: parent, Type: channelTypePrivateThread, ThreadMetadata: &ThreadMetadata{Archived: true, ArchiveTimestamp: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)}},
		}}, nil
	}
	catalog, err := discoverCatalog(t.Context(), peer, "200", config.DiscordGuildConfig{Include: []string{"301"}}, nil,
		catalogScan{full: true, publicOnly: true})
	require.NoError(err)
	assert.Equal([]string{"301"}, queriedParents)
	ids := make([]string, 0, len(catalog.Containers))
	for _, container := range catalog.Containers {
		ids = append(ids, container.Channel.ID)
	}
	assert.ElementsMatch([]string{"301", "401", "404"}, ids)
}
