package notionmeetings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type fakeImportSource struct {
	*fakeHydrationSource

	query    *QueryResult
	queryErr error
}

func (f *fakeImportSource) QueryMeetingNotes(context.Context, int) (*QueryResult, error) {
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	return f.query, nil
}

func newImporterFixture(t *testing.T) (*store.Store, *fakeImportSource, *Importer) {
	t.Helper()
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(t, err)
	source := &fakeImportSource{
		fakeHydrationSource: completeHydrationSource(),
		query:               &QueryResult{Results: []MeetingNote{hydrationMeeting()}},
	}
	imp := NewImporter(st, source)
	imp.now = func() time.Time { return time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC) }
	return st, source, imp
}

func lastNotionState(t *testing.T, st *store.Store, sourceID int64) syncState {
	t.Helper()
	run, err := st.GetLastSuccessfulSync(sourceID)
	require.NoError(t, err)
	require.True(t, run.CursorAfter.Valid)
	var state syncState
	require.NoError(t, json.Unmarshal([]byte(run.CursorAfter.String), &state))
	return state
}

func lastTranscriptRetryUntil(t *testing.T, st *store.Store, sourceID int64) map[string]string {
	t.Helper()
	run, err := st.GetLastSuccessfulSync(sourceID)
	require.NoError(t, err)
	require.True(t, run.CursorAfter.Valid)
	var cursor struct {
		TranscriptRetryUntil map[string]string `json:"transcript_retry_until"`
	}
	require.NoError(t, json.Unmarshal([]byte(run.CursorAfter.String), &cursor))
	return cursor.TranscriptRetryUntil
}

func TestImporterCreatesUpdatesAndVerifiesVisibleMeetings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)

	first, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "work", AccountEmail: "user@example.com",
	})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	assert.Equal(int64(1), first.MeetingsProcessed)
	state := lastNotionState(t, st, first.SourceID)
	assert.Equal(syncStateVersion, state.Version)
	require.Contains(state.Known, "meeting-1")
	assert.Equal("2026-08-29T10:35:00Z", state.Known["meeting-1"].LastEditedTime)

	second, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "work", AccountEmail: "user@example.com",
	})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsProcessed)
	assert.Zero(second.MeetingsUpdated)

	meeting := hydrationMeeting()
	meeting.LastEditedTime = "2026-08-29T12:30:00Z"
	source.query.Results = []MeetingNote{meeting}
	updated := paragraph("summary-1", "Ship the edited release scope.", false)
	source.blocks["summary-1"] = &updated
	third, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "work", AccountEmail: "user@example.com",
	})
	require.NoError(err)
	assert.Equal(int64(1), third.MeetingsUpdated)

	var body string
	require.NoError(st.DB().QueryRow(`SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "Ship the edited release scope.")
	var sender sql.NullInt64
	require.NoError(st.DB().QueryRow(`SELECT sender_id FROM messages`).Scan(&sender))
	assert.False(sender.Valid, "Notion created_by must not become organizer")
	var attendee string
	require.NoError(st.DB().QueryRow(`
		SELECT p.email_address
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.recipient_type = 'to'
	`).Scan(&attendee))
	assert.Equal("attendee@example.com", attendee)
	_, hits, err := st.SearchMessages("edited", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), hits)
}

func TestImporterFullRepairsDerivedArchiveData(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp := newImporterFixture(t)

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)

	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`,
	), "meeting-1").Scan(&messageID))
	for _, query := range []string{
		`UPDATE message_recipients SET email_address = NULL WHERE message_id = ?`,
		`DELETE FROM conversation_participants
		 WHERE conversation_id = (SELECT conversation_id FROM messages WHERE id = ?)`,
		`DELETE FROM message_bodies WHERE message_id = ?`,
		`UPDATE messages SET metadata = NULL WHERE id = ?`,
		`UPDATE message_raw SET raw_format = 'stale' WHERE message_id = ?`,
	} {
		_, err = st.DB().Exec(st.Rebind(query), messageID)
		require.NoError(err)
	}

	repaired, err := imp.Import(context.Background(), ImportOptions{Identifier: "work", Full: true})
	require.NoError(err)
	assert.Equal(int64(1), repaired.MeetingsUpdated)

	body, err := st.GetMessageBodyText(messageID)
	require.NoError(err)
	assert.Contains(body, "Decide the release scope.")
	metadata, err := st.GetMessageMetadata(messageID)
	require.NoError(err)
	assert.True(metadata.Valid)
	var repairedMetadata map[string]any
	require.NoError(json.Unmarshal([]byte(metadata.String), &repairedMetadata))
	assert.Equal("notion_meetings", repairedMetadata["platform"])
	var envelope, rawFormat string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mr.email_address, raw.raw_format
		FROM message_recipients mr
		JOIN message_raw raw ON raw.message_id = mr.message_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'to'
	`), messageID).Scan(&envelope, &rawFormat))
	assert.Equal("attendee@example.com", envelope)
	assert.Equal(RawFormat, rawFormat)
	var conversationParticipants int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*)
		FROM conversation_participants
		WHERE conversation_id = (SELECT conversation_id FROM messages WHERE id = ?)
	`), messageID).Scan(&conversationParticipants))
	assert.Equal(1, conversationParticipants)
}

func TestImporterRestoresMissingArchiveForKnownSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp := newImporterFixture(t)

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	_, err = st.DB().Exec(st.Rebind("DELETE FROM messages WHERE source_id = ?"), first.SourceID)
	require.NoError(err)

	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsAdded)

	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text
		FROM messages m
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_id = ? AND m.source_message_id = 'meeting-1'
	`), first.SourceID).Scan(&body))
	assert.Contains(body, "Decide the release scope.")
}

func TestImporterRepairsUnavailableRawEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *store.Store, int64)
	}{
		{
			name: "missing",
			mutate: func(t *testing.T, st *store.Store, messageID int64) {
				t.Helper()
				_, err := st.DB().Exec(st.Rebind("DELETE FROM message_raw WHERE message_id = ?"), messageID)
				require.NoError(t, err)
			},
		},
		{
			name: "malformed",
			mutate: func(t *testing.T, st *store.Store, messageID int64) {
				t.Helper()
				require.NoError(t, st.UpsertMessageRawWithFormat(messageID, []byte("{"), RawFormat))
			},
		},
		{
			name: "empty object",
			mutate: func(t *testing.T, st *store.Store, messageID int64) {
				t.Helper()
				require.NoError(t, st.UpsertMessageRawWithFormat(messageID, []byte(`{}`), RawFormat))
			},
		},
		{
			name: "corrupt compression",
			mutate: func(t *testing.T, st *store.Store, messageID int64) {
				t.Helper()
				_, err := st.DB().Exec(st.Rebind(`
					UPDATE message_raw SET raw_data = ?, compression = 'zlib'
					WHERE message_id = ?
				`), []byte("not zlib"), messageID)
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, _, imp := newImporterFixture(t)
			first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)

			var messageID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT id FROM messages
				WHERE source_id = ? AND source_message_id = 'meeting-1'
			`), first.SourceID).Scan(&messageID))
			tt.mutate(t, st, messageID)

			second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), second.MeetingsUpdated)

			raw, err := st.GetMessageRaw(messageID)
			require.NoError(err)
			var evidence rawEvidence
			require.NoError(json.Unmarshal(raw, &evidence))
			assert.Contains(evidence.Canonical.Summary, "Decide the release scope.")
		})
	}
}

func TestImporterDetectsHydratedSnapshotChangesWithoutDiscoveryEdit(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*fakeImportSource)
		wantBody string
		wantRaw  string
	}{
		{
			name: "summary child",
			mutate: func(source *fakeImportSource) {
				updated := paragraph("summary-1", "Ship the revised release scope.", false)
				source.blocks["summary-1"] = &updated
			},
			wantBody: "Ship the revised release scope.",
		},
		{
			name: "nested child",
			mutate: func(source *fakeImportSource) {
				updated := paragraph("nested-1", "Revised nested action item.", false)
				source.children["notes-1"][""] = &BlockPage{
					Results: []Block{updated},
					Raw:     json.RawMessage(`{"results":[{"id":"nested-1","text":"Revised nested action item."}],"has_more":false}`),
				}
			},
			wantBody: "Revised nested action item.",
		},
		{
			name: "page Markdown",
			mutate: func(source *fakeImportSource) {
				source.markdown.Markdown = "# Weekly planning\n\nRevised page context.\n\n## Transcript\nTest Speaker: Ready to ship."
				source.markdown.Raw = json.RawMessage(`{"object":"page_markdown","id":"page-1","markdown":"Revised page context."}`)
			},
			wantRaw: "Revised page context.",
		},
		{
			name: "attendee display label",
			mutate: func(source *fakeImportSource) {
				source.users[""].Results[1].Name = "Renamed Unresolved Attendee"
			},
			wantBody: "Attendees: Test Attendee, Renamed Unresolved Attendee",
			wantRaw:  `"attendee_labels":["Test Attendee","Renamed Unresolved Attendee"]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)

			first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), first.MeetingsAdded)

			tt.mutate(source)
			second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), second.MeetingsProcessed)
			assert.Equal(int64(1), second.MeetingsUpdated)

			var messageID int64
			var body string
			require.NoError(st.DB().QueryRow(`
				SELECT m.id, mb.body_text
				FROM messages m
				JOIN message_bodies mb ON mb.message_id = m.id
				WHERE m.source_message_id = 'meeting-1'
			`).Scan(&messageID, &body))
			if tt.wantBody != "" {
				assert.Contains(body, tt.wantBody)
			}
			if tt.wantRaw != "" {
				raw, rawErr := st.GetMessageRaw(messageID)
				require.NoError(rawErr)
				assert.Contains(string(raw), tt.wantRaw)
			}

			third, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), third.MeetingsProcessed)
			assert.Zero(third.MeetingsAdded)
			assert.Zero(third.MeetingsUpdated)
		})
	}
}

func TestImporterKeepsMatchingMeetingIDsIsolatedBySource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, firstImporter := newImporterFixture(t)
	first, err := firstImporter.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	_, err = st.GetOrCreateSource(SourceType, "personal")
	require.NoError(err)
	secondImporter := NewImporter(st, source)
	secondImporter.now = firstImporter.now
	second, err := secondImporter.Import(context.Background(), ImportOptions{Identifier: "personal"})
	require.NoError(err)
	assert.NotEqual(first.SourceID, second.SourceID)

	var count int
	require.NoError(st.DB().QueryRow(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = 'meeting-1'`,
	).Scan(&count))
	assert.Equal(2, count)
}

func TestImporterLimitAndAfterRemainLocalVisibleSetBounds(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	second := hydrationMeeting()
	second.ID = "meeting-2"
	second.LastEditedTime = "2026-08-29T10:35:00Z"
	source.query.Results = []MeetingNote{hydrationMeeting(), second}

	summary, err := imp.Import(context.Background(), ImportOptions{Identifier: "work", Limit: 1})
	require.NoError(err)
	state := lastNotionState(t, st, summary.SourceID)
	assert.True(state.Coverage.Limited)
	assert.Contains(state.Known, "meeting-1")
	assert.NotContains(state.Known, "meeting-2")

	otherStore, otherSource, otherImporter := newImporterFixture(t)
	otherSource.query.Results = []MeetingNote{hydrationMeeting()}
	after, err := otherImporter.Import(context.Background(), ImportOptions{
		Identifier: "work", CreatedAfter: time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
	})
	require.NoError(err)
	assert.Zero(after.MeetingsProcessed)
	afterState := lastNotionState(t, otherStore, after.SourceID)
	assert.Empty(afterState.Known)
}

func TestImporterRecordsPartialCoverageWithoutLooping(t *testing.T) {
	assert := assert.New(t)
	st, source, imp := newImporterFixture(t)
	source.query.HasMore = true

	summary, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(t, err)
	state := lastNotionState(t, st, summary.SourceID)
	assert.True(state.Coverage.HasMore)
	assert.Equal(1, state.Coverage.Returned)
	assert.True(summary.PartialCoverage)
}

func TestImporterHardFailureRetainsLastSuccessfulState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	before := lastNotionState(t, st, first.SourceID)

	meeting := hydrationMeeting()
	meeting.LastEditedTime = "2026-08-29T13:00:00Z"
	source.query.Results = []MeetingNote{meeting}
	delete(source.blocks, "summary-1")
	failed, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.Error(err)
	assert.Equal(int64(1), failed.Errors)
	after := lastNotionState(t, st, first.SourceID)
	assert.Equal(before, after)
}

func TestImporterReportsWriteCommittedBeforeConversationStatsFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp := newImporterFixture(t)
	if st.IsPostgreSQL() {
		_, err := st.DB().Exec(`
			CREATE FUNCTION fail_notion_meeting_stats() RETURNS trigger AS $$
			BEGIN
				RAISE EXCEPTION 'forced Notion meeting stats failure';
			END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER fail_notion_meeting_stats
			BEFORE UPDATE OF message_count ON conversations
			FOR EACH ROW EXECUTE FUNCTION fail_notion_meeting_stats();
		`)
		require.NoError(err)
	} else {
		_, err := st.DB().Exec(`
			CREATE TRIGGER fail_notion_meeting_stats
			BEFORE UPDATE OF message_count ON conversations
			BEGIN
				SELECT RAISE(ABORT, 'forced Notion meeting stats failure');
			END
		`)
		require.NoError(err)
	}

	summary, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.Error(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Equal(int64(1), summary.Errors)

	var messages int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
}

func TestImporterRetriesPendingTranscriptOutsideVisibleLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.blockErrs = map[string]error{
		"transcript-1": &APIError{Kind: ErrProvider, Status: 404, Code: "object_not_found"},
	}
	source.markdown.Markdown = "# Weekly planning"

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work", Limit: 1})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	state := lastNotionState(t, st, first.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("meeting-1", state.Pending[0].BlockID)
	var body string
	require.NoError(st.DB().QueryRow(`SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "Decide the release scope.")
	assert.NotContains(body, "Transcript:")

	fullDiscovery, err := json.Marshal(hydrationMeeting())
	require.NoError(err)
	source.blocks["meeting-1"].Raw = fullDiscovery
	source.query.Results = nil
	delete(source.blockErrs, "transcript-1")
	late := paragraph("transcript-1", "Test Speaker: Transcript arrived.", false)
	source.blocks["transcript-1"] = &late
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Transcript arrived."
	imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }

	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work", Limit: 1})
	require.NoError(err)
	assert.Equal(int64(1), second.MaintenanceRetries)
	state = lastNotionState(t, st, second.SourceID)
	assert.Empty(state.Pending)

	require.NoError(st.DB().QueryRow(`SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "Transcript arrived.")
}

func TestImporterRetriesTruncatedMarkdownTranscriptUntilComplete(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Partial transcript."
	source.markdown.Truncated = true
	source.markdown.Raw = json.RawMessage(`{"object":"page_markdown","id":"page-1","truncated":true}`)

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, first.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("meeting-1", state.Pending[0].BlockID)
	var body string
	require.NoError(st.DB().QueryRow(`SELECT body_text FROM message_bodies`).Scan(&body))
	assert.NotContains(body, "Partial transcript.")

	fullDiscovery, err := json.Marshal(hydrationMeeting())
	require.NoError(err)
	source.blocks["meeting-1"].Raw = fullDiscovery
	source.query.Results = nil
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Complete transcript."
	source.markdown.Truncated = false
	source.markdown.Raw = json.RawMessage(`{"object":"page_markdown","id":"page-1","truncated":false}`)
	imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }

	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.MaintenanceRetries)
	assert.Equal(int64(1), second.MeetingsUpdated)
	state = lastNotionState(t, st, second.SourceID)
	assert.Empty(state.Pending)
	require.NoError(st.DB().QueryRow(`SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "Complete transcript.")
}

func TestImporterDoesNotRenewExpiredUnknownEndTranscriptRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.EndTime = ""
	meeting.MeetingNotes.Recording.EndTime = ""
	source.query.Results = []MeetingNote{meeting}
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning"

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, first.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("2026-08-31T12:00:00Z", state.Pending[0].RetryUntil)

	expiredImporter := NewImporter(st, source)
	expiredImporter.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	second, err := expiredImporter.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state = lastNotionState(t, st, second.SourceID)
	assert.Empty(state.Pending)
	assert.Equal("2026-08-31T12:00:00Z", lastTranscriptRetryUntil(t, st, second.SourceID)["meeting-1"])

	muchLaterImporter := NewImporter(st, source)
	muchLaterImporter.now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
	third, err := muchLaterImporter.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state = lastNotionState(t, st, third.SourceID)
	assert.Empty(state.Pending)
	assert.Equal("2026-08-31T12:00:00Z", lastTranscriptRetryUntil(t, st, third.SourceID)["meeting-1"])

	updated := paragraph("summary-1", "Ship the post-expiry release scope.", false)
	source.blocks["summary-1"] = &updated
	editImporter := NewImporter(st, source)
	editImporter.now = func() time.Time { return time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC) }
	edited, err := editImporter.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), edited.MeetingsUpdated)
	state = lastNotionState(t, st, edited.SourceID)
	assert.Empty(state.Pending)
	var body string
	require.NoError(st.DB().QueryRow(`SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "post-expiry release scope")

	late := paragraph("transcript-1", "Test Speaker: Transcript arrived after expiry.", false)
	source.blocks["transcript-1"] = &late
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Transcript arrived after expiry."
	lateImporter := NewImporter(st, source)
	lateImporter.now = func() time.Time { return time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC) }
	lateSummary, err := lateImporter.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), lateSummary.MeetingsUpdated)
	state = lastNotionState(t, st, lateSummary.SourceID)
	assert.Empty(state.Pending)
	require.NoError(st.DB().QueryRow(`SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "Transcript arrived after expiry.")
}

func TestImporterMigratesPendingRetryDeadlineFromVersionOneCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.EndTime = ""
	source.query.Results = []MeetingNote{meeting}
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning"

	sourceRecord, err := st.GetSourceByTypeAndIdentifier(SourceType, "work")
	require.NoError(err)
	syncID, err := st.StartSync(sourceRecord.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, `{"version":1,"last_query_at":"2026-08-29T12:00:00Z","known":{},"pending":[{"block_id":"meeting-1","next_attempt_at":"2026-08-29T18:00:00Z","retry_until":"2026-08-31T12:00:00Z"}],"coverage":{"has_more":false,"returned":1}}`))

	imp.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	summary, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, summary.SourceID)
	assert.Empty(state.Pending)
	assert.Equal("2026-08-31T12:00:00Z", lastTranscriptRetryUntil(t, st, summary.SourceID)["meeting-1"])
}

func TestImporterReopensExpiredTranscriptRetryForLaterUsableEnd(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.EndTime = ""
	source.query.Results = []MeetingNote{meeting}
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning"

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	expiredImporter := NewImporter(st, source)
	expiredImporter.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	_, err = expiredImporter.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)

	meeting.MeetingNotes.CalendarEvent.EndTime = "2026-09-01T12:00:00Z"
	source.query.Results = []MeetingNote{meeting}
	extendedImporter := NewImporter(st, source)
	extendedImporter.now = func() time.Time { return time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC) }
	extended, err := extendedImporter.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, extended.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("2026-09-08T12:00:00Z", state.Pending[0].RetryUntil)
	assert.Equal("2026-09-08T12:00:00Z", lastTranscriptRetryUntil(t, st, extended.SourceID)["meeting-1"])
}

func TestSyncStateRejectsInvalidTranscriptRetryDeadline(t *testing.T) {
	var state syncState
	require.NoError(t, json.Unmarshal([]byte(`{"version":1,"known":{},"transcript_retry_until":{"meeting-1":"not-a-time"}}`), &state))

	err := state.validate()

	require.Error(t, err)
	assert.Contains(t, err.Error(), `transcript retry deadline "meeting-1" is invalid`)
}

func TestImporterContinuesWhenUserListingFailsTransiently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.usersErr = errors.New("rate limit exceeded")

	summary, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Zero(summary.Errors)

	var messageID int64
	require.NoError(st.DB().QueryRow(
		`SELECT id FROM messages WHERE source_message_id = 'meeting-1'`,
	).Scan(&messageID))
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.Contains(string(raw), "Notion User Information lookup failed: rate limit exceeded")
	var participants int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), messageID,
	).Scan(&participants))
	assert.Zero(participants)
}

func TestImporterPreservesVerifiedAttendeesWhenUserListingFailsTransiently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.users[""].Results[0].Person.Email = "durableaddress@example.com"

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)

	var messageID, recipientID int64
	var recipientEmail, recipientName, envelopeEmail string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT m.id, mr.participant_id, p.email_address, mr.display_name,
		       COALESCE(mr.email_address, '')
		FROM messages m
		JOIN message_recipients mr ON mr.message_id = m.id
		JOIN participants p ON p.id = mr.participant_id
		WHERE m.source_message_id = ? AND mr.recipient_type = 'to'
	`), "meeting-1").Scan(
		&messageID, &recipientID, &recipientEmail, &recipientName, &envelopeEmail,
	))
	assert.Equal("durableaddress@example.com", recipientEmail)
	assert.Equal("Test Attendee", recipientName)
	assert.Equal("durableaddress@example.com", envelopeEmail)

	var conversationParticipants int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*)
		FROM messages m
		JOIN conversation_participants cp ON cp.conversation_id = m.conversation_id
		JOIN participants p ON p.id = cp.participant_id
		WHERE m.id = ? AND p.email_address = ?
	`), messageID, "durableaddress@example.com").Scan(&conversationParticipants))
	assert.Equal(1, conversationParticipants)
	_, hits, err := st.SearchMessages("durableaddress", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), hits)

	survivorID, err := st.EnsureParticipant(
		"mergedaddress@example.com", "Merged Participant", "example.com",
	)
	require.NoError(err)
	require.NoError(st.MergeParticipants(recipientID, survivorID))

	source.usersErr = errors.New("rate limit exceeded")
	_, err = st.DB().Exec(st.Rebind(`
		UPDATE message_raw SET raw_data = ?, compression = 'zlib'
		WHERE message_id = ?
	`), []byte("not zlib"), messageID)
	require.NoError(err)
	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsUpdated)

	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT p.email_address, mr.display_name, COALESCE(mr.email_address, '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'to'
	`), messageID).Scan(&recipientEmail, &recipientName, &envelopeEmail))
	assert.Equal("durableaddress@example.com", recipientEmail)
	assert.Equal("Test Attendee", recipientName)
	assert.Equal("durableaddress@example.com", envelopeEmail)
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*)
		FROM conversation_participants cp
		JOIN participants p ON p.id = cp.participant_id
		WHERE cp.conversation_id = (SELECT conversation_id FROM messages WHERE id = ?)
		  AND p.email_address = ?
	`), messageID, "durableaddress@example.com").Scan(&conversationParticipants))
	assert.Equal(1, conversationParticipants)
	_, hits, err = st.SearchMessages("durableaddress", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), hits)

	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	_, validEvidence := decodeArchivedEvidence(raw, "meeting-1")
	assert.True(validEvidence)
	assert.Contains(string(raw), "preserved previously verified attendees from archived evidence")
}

func TestImporterDegradedAttendeeRecoveryDropsRemovedUsers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.users[""].Results[0].Person.Email = "retained@example.com"
	source.users[""].Results[1].Person.Email = "removed@example.com"
	source.users[""].Results[1].Person.EmailVerified = true

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)

	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1"}
	source.query.Results = []MeetingNote{meeting}
	source.usersErr = errors.New("rate limit exceeded")
	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsUpdated)

	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`,
	), "meeting-1").Scan(&messageID))
	for email, want := range map[string]int{
		"retained@example.com": 1,
		"removed@example.com":  0,
	} {
		var recipients int
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(*)
			FROM message_recipients mr
			WHERE mr.message_id = ? AND mr.email_address = ?
		`), messageID, email).Scan(&recipients))
		assert.Equal(want, recipients, email)
	}
	_, hits, err := st.SearchMessages("retained", 0, 10)
	require.NoError(err)
	assert.Equal(int64(1), hits)
	_, hits, err = st.SearchMessages("removed", 0, 10)
	require.NoError(err)
	assert.Zero(hits)
}

func TestImporterPreservesRenamedAttendeeAliasesDuringDegradedLookup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.users[""].Results[0].Person.Email = "original@example.com"

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)

	renamed := source.users[""].Results[0]
	renamed.Person.Email = "renamed@example.com"
	source.users = map[string]*UserPage{
		"": {
			Results:    []User{renamed},
			HasMore:    true,
			NextCursor: "next",
		},
	}
	source.userErrs = map[string]error{"next": errors.New("rate limit exceeded")}

	for range 2 {
		_, err = imp.Import(context.Background(), ImportOptions{Identifier: "work"})
		require.NoError(err)
	}

	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`,
	), "meeting-1").Scan(&messageID))
	for _, email := range []string{"original@example.com", "renamed@example.com"} {
		var recipients int
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT COUNT(*)
			FROM message_recipients
			WHERE message_id = ? AND email_address = ?
		`), messageID, email).Scan(&recipients))
		assert.Equal(1, recipients, email)
		_, hits, err := st.SearchMessages(strings.TrimSuffix(email, "@example.com"), 0, 10)
		require.NoError(err)
		assert.Equal(int64(1), hits, email)
	}
}

func TestImporterHealthyAttendeeResolutionRemainsAuthoritative(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeImportSource)
	}{
		{
			name: "removed from event",
			mutate: func(source *fakeImportSource) {
				meeting := hydrationMeeting()
				meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-2"}
				source.query.Results = []MeetingNote{meeting}
			},
		},
		{
			name: "now unverified",
			mutate: func(source *fakeImportSource) {
				source.users[""].Results[0].Person.EmailVerified = false
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)
			source.users[""].Results[0].Person.Email = "authoritativeaddress@example.com"

			first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), first.MeetingsAdded)

			tt.mutate(source)
			second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), second.MeetingsUpdated)

			var messageID int64
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT id FROM messages WHERE source_message_id = ?`,
			), "meeting-1").Scan(&messageID))
			var recipients int
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT COUNT(*)
				FROM message_recipients mr
				JOIN participants p ON p.id = mr.participant_id
				WHERE mr.message_id = ? AND p.email_address = ?
			`), messageID, "authoritativeaddress@example.com").Scan(&recipients))
			assert.Zero(recipients)
			var conversationParticipants int
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT COUNT(*)
				FROM conversation_participants cp
				JOIN participants p ON p.id = cp.participant_id
				WHERE cp.conversation_id = (SELECT conversation_id FROM messages WHERE id = ?)
				  AND p.email_address = ?
			`), messageID, "authoritativeaddress@example.com").Scan(&conversationParticipants))
			assert.Zero(conversationParticipants)
			_, hits, err := st.SearchMessages("authoritativeaddress", 0, 10)
			require.NoError(err)
			assert.Zero(hits)
			raw, err := st.GetMessageRaw(messageID)
			require.NoError(err)
			assert.NotContains(string(raw), "authoritativeaddress@example.com")
		})
	}
}

func TestImporterContinuesVisibleWorkWhenPendingMeetingCannotBeLoaded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning"

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, first.SourceID)
	require.Len(state.Pending, 1)

	source.blockErrs = map[string]error{
		"meeting-1": &APIError{Kind: ErrRateLimited, Status: 503, Code: "service_unavailable"},
	}
	visible := hydrationMeeting()
	visible.ID = "meeting-2"
	visible.LastEditedTime = "2026-08-29T18:01:00Z"
	visibleRaw, err := json.Marshal(visible)
	require.NoError(err)
	source.blocks["meeting-2"] = &Block{
		Object: "block", ID: "meeting-2", Type: "meeting_notes", Raw: visibleRaw,
	}
	transcript := paragraph("transcript-1", "Test Speaker: New visible meeting.", false)
	source.blocks["transcript-1"] = &transcript
	source.query.Results = []MeetingNote{visible}
	imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }

	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.Errors)
	assert.Equal(int64(1), second.MeetingsAdded)
	state = lastNotionState(t, st, second.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("meeting-1", state.Pending[0].BlockID)
	assert.Equal("2026-08-30T00:01:00Z", state.Pending[0].NextAttemptAt)
}

func TestImporterSurfacesPermanentPendingMeetingLoadFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "unauthorized", err: &APIError{Kind: ErrUnauthorized, Status: 401, Code: "unauthorized"}},
		{name: "content access", err: &APIError{Kind: ErrReadContent, Status: 403, Code: "restricted_resource"}},
		{name: "malformed response", err: fmt.Errorf("%w: invalid block", ErrMalformedResponse)},
		{name: "context canceled", err: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "# Weekly planning"

			first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			before := lastNotionState(t, st, first.SourceID)
			require.Len(before.Pending, 1)

			source.query.Results = nil
			source.blockErrs = map[string]error{"meeting-1": tt.err}
			imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }
			summary, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.Error(err)
			require.ErrorIs(err, tt.err)
			assert.Equal(int64(1), summary.Errors)

			after := lastNotionState(t, st, first.SourceID)
			assert.Equal(before.LastQueryAt, after.LastQueryAt)
			assert.Equal(before.Pending, after.Pending)
		})
	}
}

func TestImporterArchivesMetadataWhileContentIsPending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	for _, id := range []string{"summary-1", "notes-1", "transcript-1"} {
		empty := paragraph(id, "", false)
		source.blocks[id] = &empty
	}
	source.markdown.Markdown = "# Weekly planning"

	summary, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	state := lastNotionState(t, st, summary.SourceID)
	require.Contains(state.Known, "meeting-1")
	assert.NotEmpty(state.Known["meeting-1"].SnapshotSHA256)

	var body string
	require.NoError(st.DB().QueryRow(`
		SELECT mb.body_text
		FROM messages m
		JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.source_message_id = 'meeting-1'
	`).Scan(&body))
	assert.Contains(body, "Weekly planning")
	assert.Contains(body, "When: 2026-08-29 10:00 - 10:30")
	assert.NotContains(body, "Summary:")
	assert.NotContains(body, "Notes:")
	assert.NotContains(body, "Transcript:")
}

func TestImporterPreservesArchivedTranscriptOnTemporaryOmission(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *store.Store, int64)
	}{
		{name: "valid evidence"},
		{
			name: "missing evidence",
			mutate: func(t *testing.T, st *store.Store, messageID int64) {
				t.Helper()
				_, err := st.DB().Exec(st.Rebind("DELETE FROM message_raw WHERE message_id = ?"), messageID)
				require.NoError(t, err)
			},
		},
		{
			name: "malformed evidence",
			mutate: func(t *testing.T, st *store.Store, messageID int64) {
				t.Helper()
				require.NoError(t, st.UpsertMessageRawWithFormat(messageID, []byte("{"), RawFormat))
			},
		},
		{
			name: "corrupt compression",
			mutate: func(t *testing.T, st *store.Store, messageID int64) {
				t.Helper()
				_, err := st.DB().Exec(st.Rebind(`
					UPDATE message_raw SET raw_data = ?, compression = 'zlib'
					WHERE message_id = ?
				`), []byte("not zlib"), messageID)
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)
			first, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)

			var messageID int64
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT id FROM messages
				WHERE source_id = ? AND source_message_id = 'meeting-1'
			`), first.SourceID).Scan(&messageID))
			if tt.mutate != nil {
				tt.mutate(t, st, messageID)
			}

			meeting := hydrationMeeting()
			meeting.LastEditedTime = "2026-08-29T14:00:00Z"
			source.query.Results = []MeetingNote{meeting}
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "# Weekly planning"
			second, err := imp.Import(context.Background(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), second.MeetingsUpdated)

			var body string
			require.NoError(st.DB().QueryRow(`SELECT body_text FROM message_bodies`).Scan(&body))
			assert.Contains(body, "Test Speaker: Ready to ship.")
			state := lastNotionState(t, st, first.SourceID)
			require.Len(state.Pending, 1)
		})
	}
}

func TestImporterRequiresRegisteredSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := &fakeImportSource{fakeHydrationSource: completeHydrationSource(), query: &QueryResult{}}
	_, err := NewImporter(st, source).Import(context.Background(), ImportOptions{Identifier: "removed"})
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrSourceNotFound)
}

func TestPendingTranscriptCadenceAndExpiry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hydrated := &HydratedMeeting{Discovery: hydrationMeeting()}
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	pending, keep := schedulePending(hydrated, now, pendingTranscript{}, false)
	require.True(keep)
	assert.Equal("2026-08-29T18:00:00Z", pending.NextAttemptAt)
	assert.Equal("2026-09-05T10:30:00Z", pending.RetryUntil)
	assert.False(pendingDue(pending, now))
	assert.True(pendingDue(pending, now.Add(transcriptRetryCadence)))
	assert.True(pendingExpired(pending, time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC)))

	hydrated.Discovery.MeetingNotes.CalendarEvent.EndTime = ""
	hydrated.Discovery.MeetingNotes.Recording.EndTime = ""
	unknown, keep := schedulePending(hydrated, now, pendingTranscript{}, false)
	require.True(keep)
	assert.Equal("2026-08-31T12:00:00Z", unknown.RetryUntil)
}
