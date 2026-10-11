package discord

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
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

func TestPublicImportDoesNotResumePrivateThreadState(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "selected"))
	api.active = []Channel{{ID: "402", GuildID: "200", ParentID: "300", Type: channelTypePrivateThread}}
	api.messageHook = func(channel string, _ MessageQuery) ([]Message, error, bool) {
		if channel == "402" {
			return nil, errors.New("provider interrupted private thread backfill"), true
		}
		return nil, nil, false
	}
	imp := newTestImporter(st, api)
	_, err := imp.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.ErrorContains(err, "provider interrupted", "standalone collection leaves the private thread resumable")
	privateRequests := len(api.channelQueries("402"))
	require.Positive(privateRequests)

	api.messageHook = nil
	summary, err := imp.Import(t.Context(), ImportOptions{GuildID: "200", PublicChannels: []string{"300"}})
	require.NoError(err)
	assert.Len(api.channelQueries("402"), privateRequests,
		"public collection must not resume a private thread from saved state")
	assert.Empty(summary.CatalogIssues)
}

func TestPublicImportReportsUnlistedSelectedChannel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "selected"))
	imp := newTestImporter(st, api)
	summary, err := imp.Import(t.Context(), ImportOptions{GuildID: "200", PublicChannels: []string{"300", "301"}})
	require.NoError(err)
	require.Len(summary.CatalogIssues, 1)
	issue := summary.CatalogIssues[0]
	assert.Equal(CatalogIssueUnknownChannel, issue.Kind)
	assert.Equal("301", issue.ParentID)
	assert.False(issue.Fatal)
	assert.Empty(api.channelQueries("301"), "an unlisted channel is reported, not probed")
}

func TestImportRejectsSourceFromAnotherGuild(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	other, err := st.GetOrCreateSource(sourceTypeDiscord, "201")
	require.NoError(err)
	api := newImporterFakeAPI(importerTestChannel("300", "selected"))
	_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200", SourceID: other.ID})
	require.ErrorContains(err, "does not match the credential guild")
	require.Empty(api.channelQueries("300"))
}

func TestEmptyPublicSelectionCollectsNothing(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "selected"))
	summary, err := newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200", PublicChannels: []string{}})
	require.NoError(err)
	require.Zero(summary.ContainersProcessed)
	require.Empty(api.channelQueries("300"))
	_, err = st.GetLatestSyncContext(t.Context(), summary.SourceID, 0)
	require.ErrorIs(err, store.ErrSyncRunNotFound, "an empty selection starts no sync run")
}

// A library caller may sync without an attachments directory. Refreshing a
// message must still keep files that an earlier run already archived.
func TestFullImportWithoutAttachmentsDirKeepsArchivedFiles(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	api := newImporterFakeAPI(importerTestChannel("300", "selected"))
	message := importerTestMessage(importerTestSnowflake(t, time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC), 1), "300", "photo")
	message.Attachments = []Attachment{{ID: "attachment-1", Filename: "photo.png", Size: 12}}
	api.messages["300"] = []Message{message}
	imp := newTestImporter(st, api)
	_, err := imp.Import(t.Context(), ImportOptions{GuildID: "200"})
	require.NoError(err)

	hash := strings.Repeat("ab", 32)
	result, err := st.DB().Exec(st.Rebind(`UPDATE attachments SET storage_path = ?, content_hash = ?, attachment_state = 'stored'
		WHERE source_attachment_id LIKE 'discord:%'`), hash[:2]+"/"+hash, hash)
	require.NoError(err)
	rows, err := result.RowsAffected()
	require.NoError(err)
	require.Equal(int64(1), rows, "an earlier run archived the attachment")

	summary, err := imp.Import(t.Context(), ImportOptions{GuildID: "200", Full: true})
	require.NoError(err)
	assert.Zero(summary.MediaPending, "an archived file is not pending")
	var storagePath, contentHash, state string
	require.NoError(st.DB().QueryRow(`SELECT storage_path, content_hash, attachment_state FROM attachments
		WHERE source_attachment_id LIKE 'discord:%'`).Scan(&storagePath, &contentHash, &state))
	assert.Equal(hash[:2]+"/"+hash, storagePath)
	assert.Equal(hash, contentHash)
	assert.Equal("stored", state)
}
