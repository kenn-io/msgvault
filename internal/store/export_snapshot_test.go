package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestExportMessagesConcurrentWriteSnapshot(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, _, start, committed := store.ExportSnapshotFixture(t, "gmail", "mr_from")
	sink := &collectingMessageExportSink{}
	counts, err := st.ExportMessages(t.Context(), store.MessageExportFilter{Start: start, End: start.Add(time.Hour)}, sink)
	requirements.NoError(err)
	requirements.True(*committed, "the writer must commit between export phases")
	assertions.Equal(store.MessageExportCounts{Sources: 1, Conversations: 1, Messages: 1}, counts)
	requirements.Len(sink.conversations, 1)
	requirements.Len(sink.messages, 1)
	assertions.Equal("original", sink.conversations[0].ID)
	for _, message := range sink.messages {
		assertions.Equal(sink.conversations[0].ID, message.ConversationID)
	}
}

func TestDiscordExportConcurrentWriteSnapshot(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, sourceID, start, committed := store.ExportSnapshotFixture(t, "discord", "FROM messages m")
	containers, err := st.DiscordExport(t.Context(), sourceID, start, start.Add(time.Hour))
	requirements.NoError(err)
	requirements.True(*committed, "the writer must commit between export phases")
	requirements.Len(containers, 1)
	assertions.Equal("original", containers[0].ID)
	requirements.Len(containers[0].Messages, 1)
	assertions.Equal("original-message", containers[0].Messages[0].ID)
	fresh, err := st.DiscordExport(t.Context(), sourceID, start, start.Add(time.Hour))
	requirements.NoError(err)
	requirements.Len(fresh, 2)
	assertions.Equal("new-thread", fresh[0].ID)
	assertions.Equal("thread", fresh[0].Kind)
	requirements.Len(fresh[0].Messages, 1)
	assertions.Equal("new-message", fresh[0].Messages[0].ID)
}
