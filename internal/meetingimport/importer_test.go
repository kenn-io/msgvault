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
	require.NoError(t, err)

	assert.Equal(t, StatusCreated, result.Status)
	assert.Equal(t, "meeting:42", result.SourceMessageID)
	assert.NotZero(t, result.SourceID)
	assert.NotZero(t, result.MessageID)
	assert.Equal(t, 1, sourceHookCalls)
	assert.Equal(t, []string{"meeting_import:local-meetings"}, cacheLabels)

	var sourceType, identifier, displayName string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT source_type, identifier, display_name
		FROM sources WHERE id = ?
	`), result.SourceID).Scan(&sourceType, &identifier, &displayName))
	assert.Equal(t, SourceType, sourceType)
	assert.Equal(t, "local-meetings", identifier)
	assert.Equal(t, "Local Meetings", displayName)

	identities, err := st.ListAccountIdentities(result.SourceID)
	require.NoError(t, err)
	require.Len(t, identities, 1)
	assert.Equal(t, "user@example.com", identities[0].Address)
	assert.Contains(t, identities[0].SourceSignal, "account-email")

	var (
		messageType, sourceMessageID, subject string
		isFromMe                              bool
		senderEmail                           sql.NullString
		conversationType, conversationKey     string
		body, rawFormat, metadataJSON         string
		messageCount, participantCount        int
	)
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
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
	assert.Equal(t, MessageType, messageType)
	assert.Equal(t, "meeting:42", sourceMessageID)
	assert.Equal(t, "Weekly planning", subject)
	assert.False(t, isFromMe)
	assert.Equal(t, "organizer@example.com", senderEmail.String)
	assert.Equal(t, ConversationType, conversationType)
	assert.Equal(t, "meeting:42", conversationKey)
	assert.Contains(t, body, "[00:04] Steve: Let's review the launch plan.")
	assert.Equal(t, RawFormat, rawFormat)
	assert.Equal(t, 1, messageCount)
	assert.Equal(t, 1, participantCount)

	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(metadataJSON), &metadata))
	assert.Equal(t, SourceType, metadata["platform"])

	raw, err := st.GetMessageRaw(result.MessageID)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "account_email")
	assert.Contains(t, string(raw), `"external_id":"42"`)

	var fromCount, toCount, memberCount int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients
		WHERE message_id = ? AND recipient_type = 'from'
	`), result.MessageID).Scan(&fromCount))
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients
		WHERE message_id = ? AND recipient_type = 'to'
	`), result.MessageID).Scan(&toCount))
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN messages m ON m.conversation_id = cp.conversation_id
		WHERE m.id = ?
	`), result.MessageID).Scan(&memberCount))
	assert.Equal(t, 1, fromCount)
	assert.Equal(t, 1, toCount)
	assert.Equal(t, 1, memberCount)

	latest, err := st.GetLatestSync(result.SourceID)
	require.NoError(t, err)
	assert.Equal(t, store.SyncStatusCompleted, latest.Status)
	assert.Equal(t, int64(1), latest.MessagesProcessed)
	assert.Equal(t, int64(1), latest.MessagesAdded)
	assert.Equal(t, int64(0), latest.MessagesUpdated)
}

func TestImporterRetriesUpdateSameMessageAndReplacePeople(t *testing.T) {
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})

	first, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(t, err)

	second, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(t, err)
	assert.Equal(t, StatusUpdated, second.Status)
	assert.Equal(t, first.MessageID, second.MessageID)

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
	require.NoError(t, err)
	assert.Equal(t, StatusUpdated, third.Status)
	assert.Equal(t, first.MessageID, third.MessageID)

	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(t, 1, count)

	var subject, body, displayName string
	var senderID sql.NullInt64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT m.subject, mb.body_text, m.sender_id, s.display_name
		FROM messages m
		JOIN message_bodies mb ON mb.message_id = m.id
		JOIN sources s ON s.id = m.source_id
		WHERE m.id = ?
	`), first.MessageID).Scan(&subject, &body, &senderID, &displayName))
	assert.Equal(t, "Replacement title", subject)
	assert.Contains(t, body, "Replacement summary")
	assert.Contains(t, body, "Speaker 1: replacement transcript")
	assert.False(t, senderID.Valid)
	assert.Equal(t, "Renamed Meetings", displayName)

	for _, tableQuery := range []string{
		`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`,
		`SELECT COUNT(*) FROM conversation_participants cp
		 JOIN messages m ON m.conversation_id = cp.conversation_id WHERE m.id = ?`,
	} {
		require.NoError(t, st.DB().QueryRow(st.Rebind(tableQuery), first.MessageID).Scan(&count))
		assert.Equal(t, 0, count)
	}

	latest, err := st.GetLatestSync(first.SourceID)
	require.NoError(t, err)
	assert.Equal(t, store.SyncStatusCompleted, latest.Status)
	assert.Equal(t, int64(0), latest.MessagesAdded)
	assert.Equal(t, int64(1), latest.MessagesUpdated)

	identities, err := st.ListAccountIdentities(first.SourceID)
	require.NoError(t, err)
	assert.Len(t, identities, 2)
}

func TestImporterScopesExternalIDsBySource(t *testing.T) {
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	firstRequest := validImportRequest(t)
	secondRequest := validImportRequest(t)
	secondRequest.Source.Identifier = "second-stream"

	first, err := importer.Import(context.Background(), firstRequest)
	require.NoError(t, err)
	second, err := importer.Import(context.Background(), secondRequest)
	require.NoError(t, err)

	assert.NotEqual(t, first.SourceID, second.SourceID)
	assert.NotEqual(t, first.MessageID, second.MessageID)
	assert.Equal(t, first.SourceMessageID, second.SourceMessageID)
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
	require.ErrorIs(t, err, cacheErr)

	var messageID, sourceID int64
	require.NoError(t, st.DB().QueryRow(`
		SELECT id, source_id FROM messages WHERE source_message_id = 'meeting:42'
	`).Scan(&messageID, &sourceID))
	assert.NotZero(t, messageID, "message remains durable after cache failure")

	failed, err := st.GetLatestSync(sourceID)
	require.NoError(t, err)
	assert.Equal(t, store.SyncStatusFailed, failed.Status)
	assert.Equal(t, int64(1), failed.MessagesProcessed)
	assert.Equal(t, int64(1), failed.MessagesAdded)

	failCache = false
	result, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(t, err)
	assert.Equal(t, StatusUpdated, result.Status)
	assert.Equal(t, messageID, result.MessageID)

	completed, err := st.GetLatestSync(sourceID)
	require.NoError(t, err)
	assert.Equal(t, store.SyncStatusCompleted, completed.Status)
	assert.Equal(t, int64(1), completed.MessagesUpdated)
}

func TestImporterSourceHookFailureStopsBeforeSync(t *testing.T) {
	st := testutil.NewTestStore(t)
	hookErr := errors.New("synthetic migration failure")
	importer := NewImporter(st, Hooks{
		AfterSourceSetup: func() error { return hookErr },
	})

	_, err := importer.Import(context.Background(), validImportRequest(t))
	require.ErrorIs(t, err, hookErr)

	var count int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs`).Scan(&count))
	assert.Equal(t, 0, count)
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(t, 0, count)
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
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to inject a raw archive failure")
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	require.True(t, st.FTS5Available())
	_, err := st.DB().Exec(`
		CREATE TRIGGER fail_meeting_import_raw
		BEFORE INSERT ON message_raw
		WHEN NEW.raw_format = 'meeting_json'
		BEGIN
			SELECT RAISE(ABORT, 'forced meeting import raw failure');
		END
	`)
	require.NoError(t, err)

	_, err = importer.Import(context.Background(), validImportRequest(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persist meeting")

	for table, want := range map[string]int{
		"messages":           0,
		"conversations":      0,
		"message_bodies":     0,
		"message_raw":        0,
		"message_recipients": 0,
		"messages_fts":       0,
	} {
		var got int
		require.NoError(t, st.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&got), table)
		assert.Equal(t, want, got, table)
	}

	sources, err := st.ListSources(SourceType)
	require.NoError(t, err)
	require.Len(t, sources, 1)
	latest, err := st.GetLatestSync(sources[0].ID)
	require.NoError(t, err)
	assert.Equal(t, store.SyncStatusFailed, latest.Status)
	assert.Contains(t, latest.ErrorMessage.String, "persist meeting")
}

func TestImporterIndexesSubjectBodyAndAddresses(t *testing.T) {
	testutil.SkipIfPostgres(t, "asserts against the SQLite FTS5 virtual table")
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})

	result, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(t, err)

	var subject, body, fromAddr, toAddrs string
	require.NoError(t, st.DB().QueryRow(`
		SELECT subject, body, from_addr, to_addr
		FROM messages_fts WHERE rowid = ?
	`, result.MessageID).Scan(&subject, &body, &fromAddr, &toAddrs))
	assert.Equal(t, "Weekly planning", subject)
	assert.Contains(t, body, "launch plan")
	assert.Equal(t, "organizer@example.com", fromAddr)
	assert.Equal(t, "attendee@example.com", strings.TrimSpace(toAddrs))
}
