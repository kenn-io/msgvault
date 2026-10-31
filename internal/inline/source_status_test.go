package inline

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
)

func TestLimitedImportRecordsSuccessfulSourceSyncWithoutReplacingCursor(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	imp, client, opts := importerFixture(t)
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.Account.Identifier())
	must.NoError(err)
	const cursor = "opaque-source-cursor"
	must.NoError(imp.store.UpdateSourceSyncState(source.ID, cursor))
	source, err = imp.store.GetSourceByID(source.ID)
	must.NoError(err)
	must.False(source.LastSyncAt.Valid)
	client.messages[1] = []Message{syntheticMessage(1, 1, "older"), syntheticMessage(1, 2, "newer")}
	opts.Limit = 1

	summary, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	checks.Equal(1, summary.MessagesAdded)
	source, err = imp.store.GetSourceByID(source.ID)
	must.NoError(err)
	checks.True(source.LastSyncAt.Valid)
	checks.Equal(cursor, source.SyncCursor.String)
	run, err := imp.store.GetLatestSync(source.ID)
	must.NoError(err)
	checks.Equal(store.SyncStatusCompleted, run.Status)
	must.True(run.CursorAfter.Valid)
	state, err := LoadSyncState(run.CursorAfter.String, opts.Account.Identifier())
	must.NoError(err)
	chat := state.Chats[chatKey(1)]
	must.NotNil(chat)
	checks.False(chat.HistoryDone, "bounded success retains older history debt")
	checks.Equal(int64(2), chat.HistoryBefore)
}

func TestMediaBackfillRecordsSuccessfulSourceSyncAndPreservesResumeState(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	imp, client, opts := importerFixture(t)
	source, err := imp.store.GetSourceByTypeAndIdentifier(SourceType, opts.Account.Identifier())
	must.NoError(err)
	const cursor = "opaque-source-cursor"
	must.NoError(imp.store.UpdateSourceSyncState(source.ID, cursor))
	message := syntheticMessage(1, 1, "deferred photo")
	message.Media = []Media{{ID: "photo:9", ChatID: 1, MessageID: 1, Role: "photo", URL: "https://api.inline.chat/file"}}
	client.messages[1] = []Message{message}
	opts.NoMedia = true
	summary, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	must.Equal(1, summary.AttachmentsPending)
	run, err := imp.store.GetLastSuccessfulSync(source.ID)
	must.NoError(err)
	must.True(run.CursorAfter.Valid)
	resumeBlob := run.CursorAfter.String
	oldSync := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err = imp.store.DB().Exec(imp.store.Rebind(`UPDATE sources SET last_sync_at = ? WHERE id = ?`), oldSync, source.ID)
	must.NoError(err)
	content := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	imp.mediaTransport = mediaRoundTripper(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(content)), ContentLength: int64(len(content)), Request: request}, nil
	})

	backfill, err := imp.BackfillMedia(t.Context(), opts)
	must.NoError(err)
	checks.Equal(1, backfill.AttachmentsDownloaded)
	checks.Equal(1, backfill.MessagesUpdated)
	checks.Zero(backfill.MessagesAdded)
	source, err = imp.store.GetSourceByID(source.ID)
	must.NoError(err)
	must.True(source.LastSyncAt.Valid)
	checks.True(source.LastSyncAt.Time.After(oldSync))
	checks.Equal(cursor, source.SyncCursor.String)
	run, err = imp.store.GetLatestSync(source.ID)
	must.NoError(err)
	checks.Equal(store.SyncStatusCompleted, run.Status)
	checks.Equal(int64(1), run.MessagesUpdated)
	checks.Equal(resumeBlob, run.CursorAfter.String, "media work preserves the message-sync resume blob")
	messageID := archivedMessage(t, imp.store, source.ID, 1, 1)
	refs, err := imp.store.MessageInlineProviderAttachments(messageID)
	must.NoError(err)
	must.Len(refs, 1)
	checks.Equal(attachmentpolicy.StateStored, refs["inline:photo:9"].State)
}
