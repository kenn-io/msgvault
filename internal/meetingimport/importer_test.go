package meetingimport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
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

func TestImporterPostSupersessionWriteFailsGenerationFence(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL-only stale meeting writer regression")
	}
	req := validImportRequest(t)
	importer := NewImporter(st, Hooks{})
	baseline, err := importer.Import(t.Context(), req)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`DELETE FROM messages WHERE id = ?`), baseline.MessageID)
	requirements.NoError(err)

	holder, err := st.DB().Conn(t.Context())
	requirements.NoError(err)
	t.Cleanup(func() { _ = holder.Close() })
	tx, err := holder.BeginTx(t.Context(), nil)
	requirements.NoError(err)
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = tx.Rollback()
		}
	})
	_, err = tx.ExecContext(t.Context(), `LOCK TABLE messages IN ACCESS EXCLUSIVE MODE`)
	requirements.NoError(err)
	var holderPID int
	requirements.NoError(tx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&holderPID))

	type importResult struct {
		result Result
		err    error
	}
	importDone := make(chan importResult, 1)
	importFinished := make(chan struct{})
	go func() {
		defer close(importFinished)
		result, importErr := importer.Import(t.Context(), req)
		importDone <- importResult{result: result, err: importErr}
	}()
	t.Cleanup(func() {
		if locked {
			_ = tx.Rollback()
			locked = false
		}
		waitForMeetingImportGoroutine(t, importFinished)
	})
	waitForBlockedMeetingImportPID(t, st, holderPID, "SELECT source_message_id",
		"meeting importer did not pause in its post-start message lookup")

	var oldRunID int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT id FROM sync_runs
		WHERE source_id = ? AND status = 'running'
	`), baseline.SourceID).Scan(&oldRunID))
	newRunID, err := st.StartSyncContext(t.Context(), baseline.SourceID, "superseding-test-run")
	requirements.NoError(err)
	requirements.NotEqual(oldRunID, newRunID)
	requirements.NoError(tx.Commit())
	locked = false

	got := <-importDone
	requirements.ErrorIs(got.err, store.ErrSyncRunSuperseded)
	checks.Zero(got.result.MessageID)
	var messageCount int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COUNT(*) FROM messages
		WHERE source_id = ? AND source_message_id = ?
	`), baseline.SourceID, "meeting:42").Scan(&messageCount))
	checks.Zero(messageCount,
		"a meeting writer must not commit after its sync generation is superseded")
	requirements.NoError(st.FailSync(newRunID, "test cleanup"))
}

func waitForBlockedMeetingImportPID(
	t *testing.T,
	st *store.Store,
	blockerPID int,
	queryFragment string,
	message string,
) {
	t.Helper()
	var blockedPID int
	var waitErr error
	require.Eventually(t, func() bool {
		waitErr = st.DB().QueryRowContext(t.Context(), `SELECT COALESCE(MIN(pid), 0)
			FROM pg_stat_activity
			WHERE $1 = ANY(pg_blocking_pids(pid))
			  AND POSITION($2 IN query) > 0`,
			blockerPID, queryFragment).Scan(&blockedPID)
		return waitErr == nil && blockedPID > 0
	}, 5*time.Second, 10*time.Millisecond, message)
	require.NoError(t, waitErr)
}

func waitForMeetingImportGoroutine(t *testing.T, finished <-chan struct{}) {
	t.Helper()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		assert.Fail(t, "meeting import test goroutine did not finish")
	}
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

func TestImporterUnchangedRetryDoesNotPersistOrCountUpdate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	cacheCalls := 0
	importer := NewImporter(st, Hooks{
		RefreshCache: func(context.Context, string) error {
			cacheCalls++
			return nil
		},
	})

	first, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(err)

	sentinel := time.Date(2000, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE messages SET last_modified = ? WHERE id = ?
	`), sentinel, first.MessageID)
	require.NoError(err)
	var watermarkBefore string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?
	`), first.MessageID).Scan(&watermarkBefore))

	retry, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(err)
	assert.Equal(StatusUpdated, retry.Status)
	assert.Equal(first.MessageID, retry.MessageID)

	var watermarkAfter string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT CAST(last_modified AS TEXT) FROM messages WHERE id = ?
	`), first.MessageID).Scan(&watermarkAfter))
	assert.Equal(watermarkBefore, watermarkAfter)

	latest, err := st.GetLatestSync(first.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(1), latest.MessagesProcessed)
	assert.Equal(int64(0), latest.MessagesAdded)
	assert.Equal(int64(0), latest.MessagesUpdated)
	assert.Equal(2, cacheCalls)
}

func TestImporterUnchangedRetryRepairsStatsAfterPostPersistFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	testutil.SkipIfPostgres(t, "uses a SQLite trigger to inject a stats failure")
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	_, err := st.DB().Exec(`
		CREATE TRIGGER fail_meeting_import_stats
		BEFORE UPDATE OF message_count ON conversations
		BEGIN
			SELECT RAISE(ABORT, 'forced meeting import stats failure');
		END
	`)
	require.NoError(err)

	_, err = importer.Import(context.Background(), validImportRequest(t))
	require.Error(err)
	assert.Contains(err.Error(), "recompute meeting conversation stats")

	var messageID, sourceID int64
	var messageCount, participantCount int
	var preview sql.NullString
	require.NoError(st.DB().QueryRow(`
		SELECT m.id, m.source_id, c.message_count, c.participant_count,
		       c.last_message_preview
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.source_message_id = 'meeting:42'
	`).Scan(&messageID, &sourceID, &messageCount, &participantCount, &preview))
	assert.Zero(messageCount)
	assert.Zero(participantCount)
	assert.False(preview.Valid)

	_, err = st.DB().Exec(`DROP TRIGGER fail_meeting_import_stats`)
	require.NoError(err)

	retry, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(err)
	assert.Equal(StatusUpdated, retry.Status)
	assert.Equal(messageID, retry.MessageID)

	var messagePreview sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT c.message_count, c.participant_count, c.last_message_preview,
		       m.snippet
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.id = ?
	`), messageID).Scan(&messageCount, &participantCount, &preview, &messagePreview))
	assert.Equal(1, messageCount)
	assert.Equal(1, participantCount)
	assert.Equal(messagePreview, preview)

	latest, err := st.GetLatestSync(sourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(1), latest.MessagesProcessed)
	assert.Zero(latest.MessagesAdded)
	assert.Zero(latest.MessagesUpdated)
}

func TestImporterStatsRecomputeDoesNotTouchSiblingConversations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unchanged bool
		want      Status
	}{
		{name: "created", want: StatusCreated},
		{name: "unchanged", unchanged: true, want: StatusUpdated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st := testutil.NewTestStore(t)
			importer := NewImporter(st, Hooks{})
			req := validImportRequest(t)
			normalized, err := req.Normalize()
			require.NoError(err)
			if tc.unchanged {
				_, err = importer.Import(context.Background(), req)
				require.NoError(err)
			}

			source, err := st.GetOrCreateSource(SourceType, normalized.Source.Identifier)
			require.NoError(err)
			siblingID, err := st.EnsureConversationWithType(
				source.ID,
				"sibling-meeting",
				ConversationType,
				"Sibling meeting",
			)
			require.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`
				UPDATE conversations
				SET message_count = 71,
				    participant_count = 72,
				    last_message_preview = 'sibling sentinel'
				WHERE id = ?
			`), siblingID)
			require.NoError(err)

			result, err := importer.Import(context.Background(), req)
			require.NoError(err)
			assert.Equal(tc.want, result.Status)

			var messageCount, participantCount int
			var preview sql.NullString
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT message_count, participant_count, last_message_preview
				FROM conversations
				WHERE id = ?
			`), siblingID).Scan(&messageCount, &participantCount, &preview))
			assert.Equal(71, messageCount)
			assert.Equal(72, participantCount)
			assert.Equal(sql.NullString{String: "sibling sentinel", Valid: true}, preview)
		})
	}
}

func TestImporterRetryRepairsUnreadableCanonicalSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	req := validImportRequest(t)

	first, err := importer.Import(context.Background(), req)
	require.NoError(err)
	wantRaw, err := st.GetMessageRaw(first.MessageID)
	require.NoError(err)

	_, err = st.DB().Exec(st.Rebind(`
		UPDATE message_raw
		SET raw_data = ?, compression = 'zlib'
		WHERE message_id = ?
	`), []byte("not a zlib stream"), first.MessageID)
	require.NoError(err)
	_, err = st.GetMessageRaw(first.MessageID)
	require.Error(err, "corrupt fixture must be unreadable")

	retry, err := importer.Import(context.Background(), req)
	require.NoError(err)
	assert.Equal(StatusUpdated, retry.Status)
	assert.Equal(first.MessageID, retry.MessageID)

	gotRaw, err := st.GetMessageRaw(first.MessageID)
	require.NoError(err)
	assert.Equal(wantRaw, gotRaw)

	latest, err := st.GetLatestSync(first.SourceID)
	require.NoError(err)
	assert.Equal(int64(1), latest.MessagesUpdated)
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

func TestImporterAccountEmailOnlyRetryUpdatesAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	req := validImportRequest(t)

	first, err := importer.Import(context.Background(), req)
	require.NoError(err)

	var isFromMe bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT is_from_me FROM messages WHERE id = ?
	`), first.MessageID).Scan(&isFromMe))
	assert.False(isFromMe)

	req.Source.AccountEmail = req.Meeting.Organizer.Email
	retry, err := importer.Import(context.Background(), req)
	require.NoError(err)
	assert.Equal(StatusUpdated, retry.Status)
	assert.Equal(first.MessageID, retry.MessageID)

	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT is_from_me FROM messages WHERE id = ?
	`), first.MessageID).Scan(&isFromMe))
	assert.True(isFromMe)

	latest, err := st.GetLatestSync(first.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(1), latest.MessagesProcessed)
	assert.Zero(latest.MessagesAdded)
	assert.Equal(int64(1), latest.MessagesUpdated)
}

func TestImporterNewAccountEmailRefreshesEarlierMeetingAttributionForSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	req := validImportRequest(t)

	first, err := importer.Import(context.Background(), req)
	require.NoError(err)

	req.Meeting.ExternalID = "43"
	second, err := importer.Import(context.Background(), req)
	require.NoError(err)

	otherSourceReq := req
	otherSourceReq.Source.Identifier = "other-meetings"
	otherSourceReq.Meeting.ExternalID = "44"
	otherSource, err := importer.Import(context.Background(), otherSourceReq)
	require.NoError(err)

	req.Source.AccountEmail = req.Meeting.Organizer.Email
	req.Meeting.ExternalID = "45"
	confirmation, err := importer.Import(context.Background(), req)
	require.NoError(err)
	assert.Equal(StatusCreated, confirmation.Status)

	for _, messageID := range []int64{first.MessageID, second.MessageID, confirmation.MessageID} {
		var isFromMe bool
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT is_from_me FROM messages WHERE id = ?
		`), messageID).Scan(&isFromMe))
		assert.True(isFromMe, "message %d in the confirmed source", messageID)
	}

	var otherSourceIsFromMe bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT is_from_me FROM messages WHERE id = ?
	`), otherSource.MessageID).Scan(&otherSourceIsFromMe))
	assert.False(otherSourceIsFromMe)

	latest, err := st.GetLatestSync(first.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, latest.Status)
	assert.Equal(int64(1), latest.MessagesProcessed)
	assert.Equal(int64(1), latest.MessagesAdded)
	assert.Zero(latest.MessagesUpdated)
}

func TestImporterPreconfirmedAccountEmailKeepsEarlierMeetingAttributionCurrent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	req := validImportRequest(t)

	first, err := importer.Import(context.Background(), req)
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(
		first.SourceID,
		req.Meeting.Organizer.Email,
		"manual",
	))

	req.Source.AccountEmail = req.Meeting.Organizer.Email
	req.Meeting.ExternalID = "43"
	_, err = importer.Import(context.Background(), req)
	require.NoError(err)

	var earlierIsFromMe bool
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT is_from_me FROM messages WHERE id = ?
	`), first.MessageID).Scan(&earlierIsFromMe))
	assert.True(earlierIsFromMe)
}

func TestImporterCompletesSyncAndSafelyRetriesAfterCacheFailure(t *testing.T) {
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
	require.NoError(err)

	var messageID, sourceID int64
	require.NoError(st.DB().QueryRow(`
		SELECT id, source_id FROM messages WHERE source_message_id = 'meeting:42'
	`).Scan(&messageID, &sourceID))
	assert.NotZero(messageID, "message remains durable after cache failure")

	completed, err := st.GetLatestSync(sourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, completed.Status)
	assert.Equal(int64(1), completed.MessagesProcessed)
	assert.Equal(int64(1), completed.MessagesAdded)

	failCache = false
	result, err := importer.Import(context.Background(), validImportRequest(t))
	require.NoError(err)
	assert.Equal(StatusUpdated, result.Status)
	assert.Equal(messageID, result.MessageID)

	completed, err = st.GetLatestSync(sourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, completed.Status)
	assert.Equal(int64(0), completed.MessagesUpdated)
}

func TestImporterRefreshCacheDoesNotLogShutdownCancellation(t *testing.T) {
	var logs bytes.Buffer
	importer := NewImporter(nil, Hooks{
		RefreshCache: func(context.Context, string) error {
			return context.Canceled
		},
	}).WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))

	importer.refreshCache(context.Background(), "meeting_import:local-meetings", 1, "meeting:42")
	assert.Empty(t, logs.String())
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

func TestImporterCancellationDuringParticipantResolutionRollsBackParticipants(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger and registered function to pause participant insertion")
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	st.DB().SetMaxOpenConns(1)
	importer := NewImporter(st, Hooks{})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	participantStarted := make(chan struct{})
	conn, err := st.DB().Conn(context.Background())
	require.NoError(err, "get SQLite connection")
	err = conn.Raw(func(driverConn any) error {
		sqliteConn, ok := driverConn.(*sqlite3.SQLiteConn)
		require.True(ok, "driver connection is SQLite")
		return sqliteConn.RegisterFunc("wait_for_participant_cancel", func() int {
			close(participantStarted)
			<-ctx.Done()
			return 0
		}, true)
	})
	require.NoError(err, "register cancellation function")
	require.NoError(conn.Close(), "return SQLite connection to pool")
	_, err = st.DB().Exec(`
		CREATE TRIGGER wait_before_meeting_participant
		BEFORE INSERT ON participants
		WHEN NEW.email_address = 'attendee@example.com'
		BEGIN
			SELECT wait_for_participant_cancel();
		END
	`)
	require.NoError(err, "create participant cancellation trigger")

	req := validImportRequest(t)
	done := make(chan error, 1)
	go func() {
		_, importErr := importer.Import(ctx, req)
		done <- importErr
	}()

	select {
	case <-participantStarted:
	case <-time.After(time.Second):
		require.FailNow("meeting import did not reach attendee insertion")
	}
	cancel()

	select {
	case err = <-done:
	case <-time.After(time.Second):
		require.FailNow("meeting import did not stop after cancellation")
	}
	require.ErrorIs(err, context.Canceled)

	var participantCount int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*)
		FROM participants
		WHERE email_address IN ('organizer@example.com', 'attendee@example.com')
	`).Scan(&participantCount))
	assert.Zero(
		participantCount,
		"organizer and attendee inserts must roll back with canceled message persistence",
	)
	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount))
	assert.Zero(messageCount)
}

func TestImporterCancellationAtCheckpointLeavesFailedSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	checkpointStarted := make(chan struct{})
	releaseCheckpoint := make(chan struct{})
	importer := NewImporter(st, Hooks{})
	importer.beforeCheckpointForTest = func() {
		close(checkpointStarted)
		<-releaseCheckpoint
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := validImportRequest(t)
	done := make(chan error, 1)
	go func() {
		_, importErr := importer.Import(ctx, req)
		done <- importErr
	}()

	select {
	case <-checkpointStarted:
	case <-time.After(time.Second):
		require.FailNow("meeting import did not reach checkpoint")
	}
	cancel()
	close(releaseCheckpoint)

	select {
	case err := <-done:
		require.ErrorIs(err, context.Canceled)
	case <-time.After(time.Second):
		require.FailNow("meeting import did not stop after cancellation")
	}

	var messageCount int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messageCount))
	assert.Equal(1, messageCount, "canonical message is committed before checkpointing")

	sources, err := st.ListSources(SourceType)
	require.NoError(err)
	require.Len(sources, 1)
	latest, err := st.GetLatestSync(sources[0].ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.Equal(int64(1), latest.MessagesProcessed)
	assert.Equal(int64(1), latest.MessagesAdded)
	assert.Contains(latest.ErrorMessage.String, context.Canceled.Error())
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
