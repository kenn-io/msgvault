package discord

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPublicImportResumesOlderArchivedThreadAndRepairsItsMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "selected"))
	imp := newTestImporter(st, api)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	imp.now = func() time.Time { return now }
	opts := ImportOptions{GuildID: "200", PublicChannels: []string{"300"}}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err, "establish a completed baseline before new archived threads arrive")
	api.archiveHook = func(_ string, private bool, before ArchiveCursor) (ThreadPage, error) {
		assert.False(private, "the public profile never queries private archives")
		id, archivedAt := "400", now.Add(-time.Hour)
		if !before.BeforeTime.IsZero() {
			id, archivedAt = "500", now.Add(-2*time.Hour)
		}
		return ThreadPage{Threads: []Channel{{ID: id, GuildID: "200", ParentID: "300", Type: 11,
			ThreadMetadata: &ThreadMetadata{Archived: true, ArchiveTimestamp: archivedAt}}},
			HasMore: before.BeforeTime.IsZero(), NextBeforeTime: archivedAt}, nil
	}
	messageID := importerTestSnowflake(t, now.Add(-3*time.Hour), 1)
	api.messages["500"] = []Message{importerTestMessage(messageID, "500", "retained astronomy history")}
	api.messageHook = func(channel string, _ MessageQuery) ([]Message, error, bool) {
		if channel == "500" {
			return nil, errors.New("provider interrupted older thread backfill"), true
		}
		return nil, nil, false
	}
	_, err = imp.Import(t.Context(), opts)
	require.ErrorContains(err, "provider interrupted")
	api.messageHook = nil
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT count(*) FROM messages WHERE source_message_id=?`), messageID).Scan(&count))
	require.Equal(1, count, "retry must finish the older archived thread")
	api.messages["500"][0].Content = "repaired astronomy history"
	api.messages["500"][0].Raw = nil
	_, err = imp.Import(t.Context(), opts)
	require.NoError(err)
	source, err := st.GetSourceByIdentifier("200")
	require.NoError(err)
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), messageIDBySource(t, st, source.ID, messageID)).Scan(&body))
	assert.Equal("repaired astronomy history", body, "completed older threads remain in the repair walk")
}
