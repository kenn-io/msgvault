package meetingimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func validImportRequest(t *testing.T) Request {
	t.Helper()
	return decodedValidRequest(t)
}

func TestImporterCreatesCanonicalMeetingAndSyncRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	sourceHookCalls := 0
	var cacheLabels []string
	importer := NewImporter(st, Hooks{
		AfterSourceSetup: func() error {
			sourceHookCalls++
			return nil
		},
		RefreshCache: func(_ context.Context, label string) error {
			cacheLabels = append(cacheLabels, label)
			return nil
		},
	})

	result, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(err)

	assert.Equal(StatusCreated, result.Status)
	assert.Equal("meeting:42", result.SourceMessageID)
	assert.NotZero(result.SourceID)
	assert.NotZero(result.MessageID)
	assert.Equal(1, sourceHookCalls)
	assert.Equal([]string{"meeting_import:local-meetings"}, cacheLabels)

	var sourceType, identifier, displayName string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT source_type, identifier, display_name
		FROM sources WHERE id = ?
	`), result.SourceID).Scan(&sourceType, &identifier, &displayName))
	assert.Equal(SourceType, sourceType)
	assert.Equal("local-meetings", identifier)
	assert.Equal("Local Meetings", displayName)

	identities, err := st.ListAccountIdentities(result.SourceID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("user@example.com", identities[0].Address)
	assert.Contains(identities[0].SourceSignal, "account-email")

	var (
		messageType, sourceMessageID, subject string
		isFromMe                              bool
		senderEmail                           sql.NullString
		conversationType, conversationKey     string
		body, rawFormat, metadataJSON         string
		messageCount, participantCount        int
	)
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.message_type, m.source_message_id, m.subject, m.is_from_me,
		       p.email_address, c.conversation_type, c.source_conversation_id,
		       mb.body_text, mr.raw_format, m.metadata,
		       c.message_count, c.participant_count
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		JOIN message_bodies mb ON mb.message_id = m.id
		JOIN message_raw mr ON mr.message_id = m.id
		LEFT JOIN participants p ON p.id = m.sender_id
		WHERE m.id = ?
	`), result.MessageID).Scan(
		&messageType, &sourceMessageID, &subject, &isFromMe,
		&senderEmail, &conversationType, &conversationKey,
		&body, &rawFormat, &metadataJSON,
		&messageCount, &participantCount,
	))
	assert.Equal(MessageType, messageType)
	assert.Equal("meeting:42", sourceMessageID)
	assert.Equal("Weekly planning", subject)
	assert.False(isFromMe)
	assert.Equal("organizer@example.com", senderEmail.String)
	assert.Equal(ConversationType, conversationType)
	assert.Equal("meeting:42", conversationKey)
	assert.Contains(body, "[00:04] Test Speaker: Let's review the launch plan.")
	assert.Equal(RawFormat, rawFormat)
	assert.Equal(1, messageCount)
	assert.Equal(1, participantCount)

	var metadata map[string]any
	require.NoError(json.Unmarshal([]byte(metadataJSON), &metadata))
	assert.Equal(SourceType, metadata["platform"])

	raw, err := st.GetMessageRaw(result.MessageID)
	require.NoError(err)
	assert.NotContains(string(raw), "account_email")
	assert.Contains(string(raw), `"external_id":"42"`)

	var fromCount, toCount, memberCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients
		WHERE message_id = ? AND recipient_type = 'from'
	`), result.MessageID).Scan(&fromCount))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients
		WHERE message_id = ? AND recipient_type = 'to'
	`), result.MessageID).Scan(&toCount))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN messages m ON m.conversation_id = cp.conversation_id
		WHERE m.id = ?
	`), result.MessageID).Scan(&memberCount))
	assert.Equal(1, fromCount)
	assert.Equal(1, toCount)
	assert.Equal(1, memberCount)

	latest, err := st.GetLatestSync(result.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(1), latest.MessagesProcessed)
	assert.Equal(int64(1), latest.MessagesAdded)
	assert.Equal(int64(0), latest.MessagesUpdated)
}

func TestImporterRetriesUpdateSameMessageAndReplacePeople(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})

	first, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(err)

	second, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(err)
	assert.Equal(StatusUpdated, second.Status)
	assert.Equal(first.MessageID, second.MessageID)

	changed := validImportRequest(t)
	changed.Source.DisplayName = "Renamed Meetings"
	changed.Source.AccountEmail = "organizer@example.com"
	changed.Meeting.Title = "Replacement title"
	changed.Meeting.SummaryMarkdown = "Replacement summary"
	changed.Meeting.TranscriptSegments = nil
	changed.Meeting.Transcript = "Speaker 1: replacement transcript"
	changed.Meeting.Organizer = nil
	changed.Meeting.Attendees = nil

	third, err := importer.Import(context.Background(), changed)
	require.NoError(err)
	assert.Equal(StatusUpdated, third.Status)
	assert.Equal(first.MessageID, third.MessageID)

	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(1, count)

	var subject, body, displayName string
	var senderID sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.subject, mb.body_text, m.sender_id, s.display_name
		FROM messages m
		JOIN message_bodies mb ON mb.message_id = m.id
		JOIN sources s ON s.id = m.source_id
		WHERE m.id = ?
	`), first.MessageID).Scan(&subject, &body, &senderID, &displayName))
	assert.Equal("Replacement title", subject)
	assert.Contains(body, "Replacement summary")
	assert.Contains(body, "Speaker 1: replacement transcript")
	assert.False(senderID.Valid)
	assert.Equal("Renamed Meetings", displayName)

	for _, tableQuery := range []string{
		`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`,
		`SELECT COUNT(*) FROM conversation_participants cp
		 JOIN messages m ON m.conversation_id = cp.conversation_id WHERE m.id = ?`,
	} {
		require.NoError(st.DB().QueryRow(st.Rebind(tableQuery), first.MessageID).Scan(&count))
		assert.Equal(0, count)
	}

	latest, err := st.GetLatestSync(first.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(0), latest.MessagesAdded)
	assert.Equal(int64(1), latest.MessagesUpdated)

	identities, err := st.ListAccountIdentities(first.SourceID)
	require.NoError(err)
	assert.Len(identities, 2)
}

func TestImporterPreservesDisplayNameWhenRetryOmitsIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	initial := validImportRequest(t)
	initial.Source.DisplayName = "Named Meeting Source"

	created, err := importer.Import(context.Background(), initial)
	require.NoError(err)
	assert.Equal(StatusCreated, created.Status)

	retry := validImportRequest(t)
	retry.Source.DisplayName = ""
	updated, err := importer.Import(context.Background(), retry)
	require.NoError(err)
	assert.Equal(StatusUpdated, updated.Status)

	sources, err := st.ListSources(SourceType)
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal("Named Meeting Source", sources[0].DisplayName.String)
}

func TestImporterDefaultsDisplayNameOnFirstImport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	req := validImportRequest(t)
	req.Source.DisplayName = ""

	result, err := importer.Import(context.Background(), req)
	require.NoError(err)
	assert.Equal(StatusCreated, result.Status)

	sources, err := st.ListSources(SourceType)
	require.NoError(err)
	require.Len(sources, 1)
	assert.Equal("local-meetings", sources[0].DisplayName.String)
}

func TestImporterScopesExternalIDsBySource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	firstRequest := validImportRequest(t)
	secondRequest := validImportRequest(t)
	secondRequest.Source.Identifier = "second-stream"

	first, err := importer.Import(context.Background(), firstRequest)
	require.NoError(err)
	second, err := importer.Import(context.Background(), secondRequest)
	require.NoError(err)

	assert.NotEqual(first.SourceID, second.SourceID)
	assert.NotEqual(first.MessageID, second.MessageID)
	assert.Equal(first.SourceMessageID, second.SourceMessageID)
}

func TestImporterMarksOrganizerFromConfirmedAccountAsFromMe(t *testing.T) {
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	req := validImportRequest(t)
	req.Source.AccountEmail = "organizer@example.com"

	result, err := importer.Import(context.Background(), req)
	require.NoError(t, err)

	var isFromMe bool
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT is_from_me FROM messages WHERE id = ?
	`), result.MessageID).Scan(&isFromMe))
	assert.True(t, isFromMe)
}

func TestImporterMarksCacheFailureAndSafelyRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	cacheErr := errors.New("synthetic cache failure")
	failCache := true
	importer := NewImporter(st, Hooks{
		RefreshCache: func(context.Context, string) error {
			if failCache {
				return cacheErr
			}
			return nil
		},
	})

	_, err := importer.Import(context.Background(), validImportRequest(t))
	require.ErrorIs(err, cacheErr)

	var messageID, sourceID int64
	require.NoError(st.DB().QueryRow(`
		SELECT id, source_id FROM messages WHERE source_message_id = 'meeting:42'
	`).Scan(&messageID, &sourceID))
	assert.NotZero(messageID, "message remains durable after cache failure")

	failed, err := st.GetLatestSync(sourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, failed.Status)
	assert.Equal(int64(1), failed.MessagesProcessed)
	assert.Equal(int64(1), failed.MessagesAdded)

	failCache = false
	result, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(err)
	assert.Equal(StatusUpdated, result.Status)
	assert.Equal(messageID, result.MessageID)

	completed, err := st.GetLatestSync(sourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, completed.Status)
	assert.Equal(int64(1), completed.MessagesUpdated)
}

func TestImporterSourceHookFailureStopsBeforeSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	hookErr := errors.New("synthetic migration failure")
	importer := NewImporter(st, Hooks{
		AfterSourceSetup: func() error { return hookErr },
	})

	_, err := importer.Import(context.Background(), validImportRequest(t))
	require.ErrorIs(err, hookErr)

	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&count))
	assert.Equal(0, count)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(0, count)
}

func TestImporterCancellationStopsBeforeSourceSetup(t *testing.T) {
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := importer.Import(ctx, validImportRequest(t))
	require.ErrorIs(t, err, context.Canceled)

	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM sources WHERE source_type = 'meeting_import'`).Scan(&count))
	assert.Equal(t, 0, count)
}

func TestImporterRawFailureRollsBackCanonicalSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	testutil.SkipIfPostgres(t, "uses a SQLite trigger to inject a raw archive failure")
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	require.True(st.FTS5Available())
	_, err := st.DB().Exec(`
		CREATE TRIGGER fail_meeting_import_raw
		BEFORE INSERT ON message_raw
		WHEN NEW.raw_format = 'meeting_json'
		BEGIN
			SELECT RAISE(ABORT, 'forced meeting import raw failure');
		END
	`)
	require.NoError(err)

	_, err = importer.Import(context.Background(), validImportRequest(t))
	require.Error(err)
	assert.Contains(err.Error(), "persist meeting")

	for table, want := range map[string]int{
		"messages":           0,
		"conversations":      0,
		"message_bodies":     0,
		"message_raw":        0,
		"message_recipients": 0,
		"messages_fts":       0,
	} {
		var got int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&got), table)
		assert.Equal(want, got, table)
	}

	sources, err := st.ListSources(SourceType)
	require.NoError(err)
	require.Len(sources, 1)
	latest, err := st.GetLatestSync(sources[0].ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.Contains(latest.ErrorMessage.String, "persist meeting")
}

func TestImporterIndexesSubjectBodyAndAddresses(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	testutil.SkipIfPostgres(t, "asserts against the SQLite FTS5 virtual table")
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})

	result, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(err)

	var subject, body, fromAddr, toAddrs string
	require.NoError(st.DB().QueryRow(`
		SELECT subject, body, from_addr, to_addr
		FROM messages_fts WHERE rowid = ?
	`, result.MessageID).Scan(&subject, &body, &fromAddr, &toAddrs))
	assert.Equal("Weekly planning", subject)
	assert.Contains(body, "launch plan")
	assert.Equal("organizer@example.com", fromAddr)
	assert.Equal("attendee@example.com", strings.TrimSpace(toAddrs))
}
