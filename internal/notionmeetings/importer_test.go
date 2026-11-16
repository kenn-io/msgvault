package notionmeetings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
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
	result := *f.query
	result.Results = append([]MeetingNote(nil), f.query.Results...)
	for i := range result.Results {
		raw, err := json.Marshal(result.Results[i])
		if err != nil {
			return nil, err
		}
		result.Results[i].Raw = raw
	}
	return &result, nil
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

	first, err := imp.Import(t.Context(), ImportOptions{
		Identifier: "work", AccountEmail: "user@example.com",
	})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	assert.Equal(int64(1), first.MeetingsProcessed)
	state := lastNotionState(t, st, first.SourceID)
	assert.Equal(syncStateVersion, state.Version)
	require.Contains(state.Known, "meeting-1")
	assert.Equal("2026-08-29T10:35:00Z", state.Known["meeting-1"].LastEditedTime)

	second, err := imp.Import(t.Context(), ImportOptions{
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
	third, err := imp.Import(t.Context(), ImportOptions{
		Identifier: "work", AccountEmail: "user@example.com",
	})
	require.NoError(err)
	assert.Equal(int64(1), third.MeetingsUpdated)

	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "Ship the edited release scope.")
	var sender sql.NullInt64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages`).Scan(&sender))
	assert.False(sender.Valid, "Notion created_by must not become organizer")
	var attendee string
	require.NoError(st.DB().QueryRowContext(t.Context(), `
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

	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)

	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
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
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(query), messageID)
		require.NoError(err)
	}

	repaired, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", Full: true})
	require.NoError(err)
	assert.Equal(int64(1), repaired.MeetingsUpdated)

	body, err := st.GetMessageBodyText(t.Context(), messageID)
	require.NoError(err)
	assert.Contains(body, "Decide the release scope.")
	metadata, err := st.GetMessageMetadata(messageID)
	require.NoError(err)
	assert.True(metadata.Valid)
	var repairedMetadata map[string]any
	require.NoError(json.Unmarshal([]byte(metadata.String), &repairedMetadata))
	assert.Equal("notion_meetings", repairedMetadata["platform"])
	var envelope, rawFormat string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT mr.email_address, raw.raw_format
		FROM message_recipients mr
		JOIN message_raw raw ON raw.message_id = mr.message_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'to'
	`), messageID).Scan(&envelope, &rawFormat))
	assert.Equal("attendee@example.com", envelope)
	assert.Equal(RawFormat, rawFormat)
	var conversationParticipants int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
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

	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("DELETE FROM messages WHERE source_id = ?"), first.SourceID)
	require.NoError(err)

	second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsAdded)

	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
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
				_, err := st.DB().ExecContext(t.Context(), st.Rebind("DELETE FROM message_raw WHERE message_id = ?"), messageID)
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
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(`
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
			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)

			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
				SELECT id FROM messages
				WHERE source_id = ? AND source_message_id = 'meeting-1'
			`), first.SourceID).Scan(&messageID))
			tt.mutate(t, st, messageID)

			second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
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

func TestImporterRetriesIncompleteAfterFailure(t *testing.T) {
	for _, tc := range []struct {
		name                                                string
		archive, deadline, anotherMeeting, markdown, cancel bool
		triggerColumn, triggerCondition                     string
	}{
		{name: "another meeting", anotherMeeting: true},
		{name: "markdown", markdown: true},
		{name: "archive", archive: true},
		{name: "archive deadline", archive: true, deadline: true},
		{name: "checkpoint", triggerColumn: "cursor_before", triggerCondition: "NEW.status = 'running'"},
		{name: "completion and failure", triggerColumn: "status", triggerCondition: "NEW.status IN ('completed', 'failed')"},
		{name: "cancelled before archive", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, source, imp := newImporterFixture(t)
			limited := &APIError{Kind: ErrRateLimited, Status: 429, Code: "rate_limited"}
			if tc.deadline {
				meeting := hydrationMeeting()
				meeting.MeetingNotes.CalendarEvent.EndTime = ""
				raw, err := json.Marshal(meeting)
				require.NoError(t, err)
				source.blocks["meeting-1"].Raw = raw
				source.query.Results = []MeetingNote{meeting}
				empty := paragraph("transcript-1", "", false)
				source.blocks["transcript-1"] = &empty
				source.markdown.Markdown = ""
				source.markdown.Raw = json.RawMessage(`{"object":"page_markdown","id":"page-1","markdown":""}`)
			}
			if tc.archive {
				source.blockErrs = map[string]error{"summary-1": limited}
			}
			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(t, err)
			if tc.deadline {
				require.Len(t, lastNotionState(t, st, first.SourceID).Pending, 1)
			}
			if tc.archive && !tc.deadline {
				source.query.Results = nil
				source.blockCalls = nil
				imp.now = func() time.Time { return time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC) }
				notDue, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
				require.NoError(t, err)
				assert.True(t, notDue.PartialCoverage)
				assert.Empty(t, source.blockCalls)
			}
			if tc.anotherMeeting {
				second := hydrationMeeting()
				second.ID = "meeting-2"
				source.query.Results = append(source.query.Results, second)
			}
			if !tc.archive {
				source.blockErrs = map[string]error{"summary-1": limited}
				if tc.anotherMeeting {
					source.blockErrs["meeting-2"] = errors.New("forced meeting retrieval failure")
				}
				if tc.markdown {
					empty := paragraph("transcript-1", "", false)
					source.blocks["transcript-1"] = &empty
					source.markdownErr = &APIError{Kind: ErrUnauthorized, Status: 401, Code: "unauthorized"}
				}
				if tc.triggerColumn != "" {
					column, condition := tc.triggerColumn, tc.triggerCondition
					if st.IsPostgreSQL() {
						_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION fail_notion_final_write() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'forced final write failure'; END; $$ LANGUAGE plpgsql; CREATE TRIGGER fail_notion_final_write BEFORE UPDATE OF %s ON sync_runs FOR EACH ROW WHEN (%s) EXECUTE FUNCTION fail_notion_final_write()`, column, condition))
					} else {
						_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_notion_final_write BEFORE UPDATE OF %s ON sync_runs WHEN %s BEGIN SELECT RAISE(ABORT, 'forced final write failure'); END`, column, condition))
					}
					require.NoError(t, err)
				}
				ctx := t.Context()
				if tc.cancel {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					defer cancel()
					source.onBlock = func(id string) {
						if id == "transcript-1" {
							cancel()
						}
					}
				}
				_, err = imp.Import(ctx, ImportOptions{Identifier: "work"})
				if tc.cancel {
					require.ErrorIs(t, err, context.Canceled)
					checkpoint, checkpointErr := st.GetLatestCheckpointedSyncByType(t.Context(), first.SourceID, SourceType)
					require.NoError(t, checkpointErr)
					var state syncState
					require.NoError(t, json.Unmarshal([]byte(checkpoint.CursorBefore.String), &state))
					require.Len(t, state.Pending, 1)
					assert.Equal(t, "meeting-1", state.Pending[0].BlockID)
					assert.True(t, state.Pending[0].Incomplete)
					source.onBlock = nil
				}
				if tc.triggerColumn != "" {
					require.ErrorContains(t, err, "forced final write failure")
					if st.IsPostgreSQL() {
						_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_notion_final_write ON sync_runs`)
					} else {
						_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_notion_final_write`)
					}
					require.NoError(t, err)
					checkpoint, err := st.GetLatestCheckpointedSyncByType(t.Context(), first.SourceID, SourceType)
					require.NoError(t, err)
					var state syncState
					require.NoError(t, json.Unmarshal([]byte(checkpoint.CursorBefore.String), &state))
					assert.Equal(t, "2026-09-05T10:30:00Z", state.TranscriptRetryUntil["meeting-1"])
				} else {
					require.Error(t, err)
				}
			}
			source.query.Results = nil
			delete(source.blockErrs, "summary-1")
			source.markdownErr = nil
			updated := paragraph("summary-1", "Recovered summary.", false)
			source.blocks["summary-1"] = &updated
			imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }
			if tc.archive {
				if tc.deadline {
					imp.now = func() time.Time { return time.Date(2026, 8, 31, 11, 0, 0, 0, time.UTC) }
				}
				if st.IsPostgreSQL() {
					_, err = st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_notion_retry() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'forced archive failure'; END; $$ LANGUAGE plpgsql; CREATE TRIGGER fail_notion_retry BEFORE UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION fail_notion_retry()`)
				} else {
					_, err = st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_notion_retry BEFORE UPDATE ON messages BEGIN SELECT RAISE(ABORT, 'forced archive failure'); END`)
				}
				require.NoError(t, err)
				_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work"})
				require.ErrorContains(t, err, "forced archive failure")
				if st.IsPostgreSQL() {
					_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_notion_retry ON messages`)
				} else {
					_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_notion_retry`)
				}
				require.NoError(t, err)
				if tc.deadline {
					imp.now = func() time.Time { return time.Date(2026, 8, 31, 12, 1, 0, 0, time.UTC) }
				}
			}
			result, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(t, err)
			assert.Equal(t, int64(1), result.MaintenanceRetries)
			var messageID int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_id = ?`), first.SourceID).Scan(&messageID))
			body, err := st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(t, err)
			assert.Contains(t, body, "Recovered summary.")
			assert.Empty(t, lastNotionState(t, st, first.SourceID).Pending)
		})
	}
}

func TestImporterDetectsHydratedSnapshotChangesWithoutDiscoveryEdit(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*fakeImportSource)
		wantBody        string
		wantRaw         string
		emptyTranscript bool
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
			name:            "page Markdown",
			emptyTranscript: true,
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
			if tt.emptyTranscript {
				empty := paragraph("transcript-1", "", false)
				source.blocks["transcript-1"] = &empty
			}

			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), first.MeetingsAdded)

			tt.mutate(source)
			second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), second.MeetingsProcessed)
			assert.Equal(int64(1), second.MeetingsUpdated)

			var messageID int64
			var body string
			require.NoError(st.DB().QueryRowContext(t.Context(), `
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

			third, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
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
	first, err := firstImporter.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	_, err = st.GetOrCreateSource(SourceType, "personal")
	require.NoError(err)
	secondImporter := NewImporter(st, source)
	secondImporter.now = firstImporter.now
	second, err := secondImporter.Import(t.Context(), ImportOptions{Identifier: "personal"})
	require.NoError(err)
	assert.NotEqual(first.SourceID, second.SourceID)

	var count int
	require.NoError(st.DB().QueryRowContext(
		t.Context(),
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

	summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", Limit: 1})
	require.NoError(err)
	state := lastNotionState(t, st, summary.SourceID)
	assert.True(state.Coverage.Limited)
	assert.Contains(state.Known, "meeting-1")
	assert.NotContains(state.Known, "meeting-2")

	otherStore, otherSource, otherImporter := newImporterFixture(t)
	otherSource.query.Results = []MeetingNote{hydrationMeeting()}
	after, err := otherImporter.Import(t.Context(), ImportOptions{
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

	summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
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
	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	before := lastNotionState(t, st, first.SourceID)

	meeting := hydrationMeeting()
	meeting.LastEditedTime = "2026-08-29T13:00:00Z"
	source.query.Results = []MeetingNote{meeting}
	delete(source.blocks, "summary-1")
	failed, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
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
		_, err := st.DB().ExecContext(t.Context(), `
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
		_, err := st.DB().ExecContext(t.Context(), `
			CREATE TRIGGER fail_notion_meeting_stats
			BEFORE UPDATE OF message_count ON conversations
			BEGIN
				SELECT RAISE(ABORT, 'forced Notion meeting stats failure');
			END
		`)
		require.NoError(err)
	}

	summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.Error(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Equal(int64(1), summary.Errors)

	var messages int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
}

func TestImporterUsesPageTranscriptWhenBlockUnavailable(t *testing.T) {
	st, source, imp := newImporterFixture(t)
	source.blockErrs = map[string]error{"transcript-1": &APIError{Kind: ErrProvider, Status: 404, Code: "object_not_found"}}
	source.markdown.Markdown = "# Transcript\nPage transcript"
	var err error
	source.markdown.Raw, err = json.Marshal(source.markdown)
	require.NoError(t, err)

	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(t, err)
	assert.True(t, first.PartialCoverage)
	pending := lastNotionState(t, st, first.SourceID).Pending
	require.Len(t, pending, 1)
	assert.True(t, pending[0].Incomplete)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&id))
	raw, err := st.GetMessageRaw(id)
	require.NoError(t, err)
	assert.Equal(t, "Page transcript", meetingcontent.Decode(RawFormat, raw, nil).Transcript.Text)
}

func TestImporterKeepsDataSourceSnapshotWhenMergedEvidenceExceedsLimit(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(t, err)
	meeting := hydrationMeeting()
	meeting.Parent = Parent{Type: "data_source_id"}
	meeting.MeetingNotes.CalendarEvent.Attendees = nil
	meeting.MeetingNotes.Children.TranscriptBlockID = ""
	meetingRaw, err := json.Marshal(meeting)
	require.NoError(t, err)
	var fixture map[string]any
	require.NoError(t, json.Unmarshal(meetingRaw, &fixture))
	fixture["parent"] = map[string]string{"type": "data_source_id", "data_source_id": "source-1"}
	meetingRaw, err = json.Marshal(fixture)
	require.NoError(t, err)
	seven := strings.Repeat("s", 7<<20)
	ten := strings.Repeat("n", 10<<20)
	second := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var response string
		switch r.URL.Path {
		case "/v1/blocks/meeting_notes/query":
			response = `{"results":[` + string(meetingRaw) + `],"has_more":false}`
		case "/v1/blocks/meeting-1":
			response = string(meetingRaw)
		case "/v1/users":
			response = `{"results":[],"has_more":false}`
		case "/v1/blocks/summary-1", "/v1/blocks/notes-1":
			id := strings.TrimPrefix(r.URL.Path, "/v1/blocks/")
			if id == "summary-1" && second {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"object":"error","status":404,"code":"object_not_found"}`))
				return
			}
			block := paragraph(id, "", true)
			response = string(block.Raw)
		case "/v1/blocks/summary-1/children", "/v1/blocks/notes-1/children":
			id := strings.Split(r.URL.Path, "/")[3]
			cursor := r.URL.Query().Get("start_cursor")
			index := 0
			if cursor != "" {
				index, _ = strconv.Atoi(cursor)
			}
			count, content := 2, seven
			if second && id == "notes-1" {
				count, content = 3, ten
			}
			block := paragraph(fmt.Sprintf("%s-child-%d", id, index), content, false)
			response = fmt.Sprintf(`{"results":[%s],"has_more":%t,"next_cursor":"%d"}`, block.Raw, index+1 < count, index+1)
		default:
			assert.Fail(t, "unexpected request", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		assert.Less(t, int64(len(response)), maxResponseSize)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	imp := NewImporter(st, NewClient(server.URL, "ntn_test"))
	imp.now = func() time.Time { return time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC) }
	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(t, err)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&id))
	before, err := st.GetMessageRaw(id)
	require.NoError(t, err)
	assert.Equal(t, meetingcontent.StateAvailable, meetingcontent.Decode(RawFormat, before, nil).Summary.State)
	checksum := checksumBytes(before)
	second = true
	result, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(t, err)
	assert.True(t, result.PartialCoverage)
	raw, err := st.GetMessageRaw(id)
	require.NoError(t, err)
	assert.Equal(t, checksum, checksumBytes(raw))
	assert.Equal(t, meetingcontent.StateAvailable, meetingcontent.Decode(RawFormat, raw, nil).Summary.State)
	assert.Len(t, lastNotionState(t, st, first.SourceID).Pending, 1)
}

func TestImporterStopsAndReopensExpiredTranscriptRetry(t *testing.T) {
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

	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, first.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("2026-08-31T12:00:00Z", state.Pending[0].RetryUntil)

	expiredImporter := NewImporter(st, source)
	expiredImporter.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	second, err := expiredImporter.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state = lastNotionState(t, st, second.SourceID)
	assert.Empty(state.Pending)
	assert.Equal("2026-08-31T12:00:00Z", lastTranscriptRetryUntil(t, st, second.SourceID)["meeting-1"])

	muchLaterImporter := NewImporter(st, source)
	muchLaterImporter.now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
	third, err := muchLaterImporter.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state = lastNotionState(t, st, third.SourceID)
	assert.Empty(state.Pending)
	assert.Equal("2026-08-31T12:00:00Z", lastTranscriptRetryUntil(t, st, third.SourceID)["meeting-1"])

	updated := paragraph("summary-1", "Ship the post-expiry release scope.", false)
	source.blocks["summary-1"] = &updated
	editImporter := NewImporter(st, source)
	editImporter.now = func() time.Time { return time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC) }
	edited, err := editImporter.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), edited.MeetingsUpdated)
	state = lastNotionState(t, st, edited.SourceID)
	assert.Empty(state.Pending)
	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "post-expiry release scope")

	source.query.Results[0].MeetingNotes.CalendarEvent.EndTime = "2026-09-10T10:00:00Z"
	full, err := editImporter.Import(t.Context(), ImportOptions{Identifier: "work", Full: true})
	require.NoError(err)
	pending := lastNotionState(t, st, full.SourceID).Pending
	require.Len(pending, 1)
	assert.Equal("2026-09-17T10:00:00Z", pending[0].RetryUntil)

	late := paragraph("transcript-1", "Test Speaker: Transcript arrived after expiry.", false)
	source.blocks["transcript-1"] = &late
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Transcript arrived after expiry."
	lateImporter := NewImporter(st, source)
	lateImporter.now = func() time.Time { return time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC) }
	lateSummary, err := lateImporter.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), lateSummary.MeetingsUpdated)
	state = lastNotionState(t, st, lateSummary.SourceID)
	assert.Empty(state.Pending)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
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

	sourceRecord, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	require.NoError(err)
	syncID, err := st.StartSync(sourceRecord.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, `{"version":1,"last_query_at":"2026-08-29T12:00:00Z","known":{},"pending":[{"block_id":"meeting-1","next_attempt_at":"2026-08-29T18:00:00Z","retry_until":"2026-08-31T12:00:00Z"}],"coverage":{"has_more":false,"returned":1}}`))

	imp.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, summary.SourceID)
	assert.Empty(state.Pending)
	assert.Equal("2026-08-31T12:00:00Z", lastTranscriptRetryUntil(t, st, summary.SourceID)["meeting-1"])
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

	summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Zero(summary.Errors)

	var messageID int64
	require.NoError(st.DB().QueryRowContext(
		t.Context(),
		`SELECT id FROM messages WHERE source_message_id = 'meeting-1'`,
	).Scan(&messageID))
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.Contains(string(raw), "Notion User Information lookup failed: rate limit exceeded")
	var participants int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`), messageID,
	).Scan(&participants))
	assert.Zero(participants)
}

func TestImporterPreservesVerifiedAttendeesWhenUserListingFailsTransiently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.users[""].Results[0].Person.Email = "durableaddress@example.com"

	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)

	var messageID, recipientID int64
	var recipientEmail, recipientName, envelopeEmail string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
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
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
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
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE message_raw SET raw_data = ?, compression = 'zlib'
		WHERE message_id = ?
	`), []byte("not zlib"), messageID)
	require.NoError(err)
	second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsUpdated)

	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT p.email_address, mr.display_name, COALESCE(mr.email_address, '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'to'
	`), messageID).Scan(&recipientEmail, &recipientName, &envelopeEmail))
	assert.Equal("durableaddress@example.com", recipientEmail)
	assert.Equal("Test Attendee", recipientName)
	assert.Equal("durableaddress@example.com", envelopeEmail)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
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
	_, _, validEvidence, err := decodeArchivedEvidence(raw, "meeting-1")
	require.NoError(err)
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

	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)

	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1"}
	source.query.Results = []MeetingNote{meeting}
	source.usersErr = errors.New("rate limit exceeded")
	second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsUpdated)

	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`,
	), "meeting-1").Scan(&messageID))
	for email, want := range map[string]int{
		"retained@example.com": 1,
		"removed@example.com":  0,
	} {
		var recipients int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
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

	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
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
		_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work"})
		require.NoError(err)
	}

	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`,
	), "meeting-1").Scan(&messageID))
	for _, email := range []string{"original@example.com", "renamed@example.com"} {
		var recipients int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
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

			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), first.MeetingsAdded)

			tt.mutate(source)
			second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), second.MeetingsUpdated)

			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
				`SELECT id FROM messages WHERE source_message_id = ?`,
			), "meeting-1").Scan(&messageID))
			var recipients int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
				SELECT COUNT(*)
				FROM message_recipients mr
				JOIN participants p ON p.id = mr.participant_id
				WHERE mr.message_id = ? AND p.email_address = ?
			`), messageID, "authoritativeaddress@example.com").Scan(&recipients))
			assert.Zero(recipients)
			var conversationParticipants int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
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
	for _, tc := range []struct {
		name               string
		err                error
		wantErr, permanent bool
	}{
		{"service unavailable", &APIError{Kind: ErrRateLimited, Status: 503, Code: "service_unavailable"}, false, false},
		{"not found", &APIError{Kind: ErrProvider, Status: 404, Code: "object_not_found"}, true, false},
		{"unauthorized", &APIError{Kind: ErrUnauthorized, Status: 401, Code: "unauthorized"}, true, true},
		{"content access", &APIError{Kind: ErrReadContent, Status: 403, Code: "restricted_resource"}, true, true},
		{"malformed response", fmt.Errorf("%w: invalid block", ErrMalformedResponse), true, true},
		{"context canceled", context.Canceled, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "# Weekly planning"

			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			state := lastNotionState(t, st, first.SourceID)
			require.Len(state.Pending, 1)

			before := state
			originalDeadline := state.Pending[0].RetryUntil
			source.blockErrs = map[string]error{
				"meeting-1": tc.err,
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

			if tc.permanent {
				source.query.Results = nil
			}
			var progress []string
			second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work", Progress: func(s string) { progress = append(progress, s) }})
			if tc.wantErr {
				require.ErrorIs(err, tc.err)
				if tc.permanent {
					after := lastNotionState(t, st, first.SourceID)
					assert.Equal(before.LastQueryAt, after.LastQueryAt)
					assert.Equal(before.Pending, after.Pending)
					assert.Equal(int64(1), second.Errors)
					return
				}
				checkpoint, err := st.GetLatestCheckpointedSyncByType(t.Context(), second.SourceID, SourceType)
				require.NoError(err)
				require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &state))
			} else {
				require.NoError(err)
				state = lastNotionState(t, st, second.SourceID)
			}
			assert.Equal(int64(1), second.Errors)
			assert.Equal(int64(1), second.MeetingsAdded)
			require.Len(state.Pending, 1)
			assert.Equal("meeting-1", state.Pending[0].BlockID)
			assert.Equal("2026-08-30T00:01:00Z", state.Pending[0].NextAttemptAt)
			assert.Equal(originalDeadline, state.Pending[0].RetryUntil)
			assert.Contains(strings.Join(progress, "\n"), "load failed; retry deferred")
			delete(source.blockErrs, "meeting-1")
			source.query.Results = nil
			imp.now = func() time.Time { return time.Date(2026, 8, 30, 0, 2, 0, 0, time.UTC) }
			third, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), third.MaintenanceRetries)
			assert.Empty(lastNotionState(t, st, third.SourceID).Pending)
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

	summary, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	state := lastNotionState(t, st, summary.SourceID)
	require.Contains(state.Known, "meeting-1")
	assert.NotEmpty(state.Known["meeting-1"].SnapshotSHA256)

	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), `
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

func TestImporterRequiresRegisteredSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := &fakeImportSource{fakeHydrationSource: completeHydrationSource(), query: &QueryResult{}}
	_, err := NewImporter(st, source).Import(t.Context(), ImportOptions{Identifier: "removed"})
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrSourceNotFound)
}

func TestPendingTranscriptCadenceAndExpiry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	hydrated := &HydratedMeeting{Discovery: hydrationMeeting()}
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	pending, keep := schedulePending(hydrated, now, pendingTranscript{}, false, false)
	require.True(keep)
	assert.Equal("2026-08-29T18:00:00Z", pending.NextAttemptAt)
	assert.Equal("2026-09-05T10:30:00Z", pending.RetryUntil)
	assert.False(pendingDue(pending, now))
	assert.True(pendingDue(pending, now.Add(transcriptRetryCadence)))
	assert.True(pendingExpired(pending, time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC)))

	hydrated.Discovery.MeetingNotes.CalendarEvent.EndTime = ""
	hydrated.Discovery.MeetingNotes.Recording.EndTime = ""
	unknown, keep := schedulePending(hydrated, now, pendingTranscript{}, false, false)
	require.True(keep)
	assert.Equal("2026-08-31T12:00:00Z", unknown.RetryUntil)
	expired := pendingTranscript{RetryUntil: "2026-08-28T12:00:00Z"}
	renewed, keep := schedulePending(hydrated, now, expired, true, true)
	assert.True(keep)
	assert.Equal("2026-08-31T12:00:00Z", renewed.RetryUntil)
}

func TestImporterRetriesIncompleteSections(t *testing.T) {
	for _, tc := range []struct {
		name, section                 string
		emptySummary, missingEvidence bool
	}{
		{name: "transcript", section: "transcript"}, {name: "summary", section: "summary"}, {name: "notes", section: "notes"},
		{name: "saved empty summary", section: "summary", emptySummary: true},
		{name: "transcript without evidence", section: "transcript", missingEvidence: true},
	} {
		section := tc.section
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)
			if tc.emptySummary {
				empty := paragraph("summary-1", "", false)
				source.blocks["summary-1"] = &empty
			}
			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), first.MeetingsAdded)
			assert.False(first.PartialCoverage)
			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
			before, err := st.GetMessageRaw(messageID)
			require.NoError(err)
			var beforeEvidence map[string]json.RawMessage
			require.NoError(json.Unmarshal(before, &beforeEvidence))
			if tc.missingEvidence {
				require.NoError(st.UpsertMessageRawWithFormat(messageID, []byte("{"), RawFormat))
			}
			id := section + "-1"
			source.blockErrs = map[string]error{id: &APIError{Kind: ErrRateLimited, Status: 429, Code: "rate_limited"}}
			if section == "transcript" {
				source.blockErrs[id] = &APIError{Kind: ErrProvider, Status: 404, Code: "object_not_found"}
				source.markdown.Markdown = "# Transcript\nDifferent page transcript"
				source.markdown.Raw, err = json.Marshal(source.markdown)
				require.NoError(err)
			}

			for _, other := range []string{"summary", "notes", "transcript"} {
				if other != section {
					updated := paragraph(other+"-1", "Updated "+other, false)
					source.blocks[other+"-1"] = &updated
				}
			}
			second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.True(second.PartialCoverage)
			require.Len(lastNotionState(t, st, second.SourceID).Pending, 1)
			if section == "transcript" {
				fullDiscovery, err := json.Marshal(hydrationMeeting())
				require.NoError(err)
				source.blocks["meeting-1"].Raw = fullDiscovery
				source.query.Results = nil
			}
			repeated, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.Zero(repeated.MeetingsUpdated)
			updatedRaw, err := st.GetMessageRaw(messageID)
			require.NoError(err)
			var afterEvidence map[string]json.RawMessage
			require.NoError(json.Unmarshal(updatedRaw, &afterEvidence))
			if !tc.missingEvidence {
				assert.JSONEq(string(beforeEvidence[section]), string(afterEvidence[section]))
			}
			body, err := st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(err)
			original := map[string]string{"summary": "Decide the release scope.", "notes": "Owner will prepare the rollout.", "transcript": "Test Speaker: Ready to ship."}
			if section == "transcript" {
				assert.Contains(body, original[section])
				assert.NotContains(body, "Different page transcript")
				if !tc.missingEvidence {
					assert.JSONEq(string(beforeEvidence["page_markdown"]), string(afterEvidence["page_markdown"]))
				}
				metadata, err := st.GetMessageMetadata(messageID)
				require.NoError(err)
				require.True(metadata.Valid)
				var fields map[string]any
				require.NoError(json.Unmarshal([]byte(metadata.String), &fields))
				assert.NotContains(fields, "markdown_transcript_fallback")
			} else if !tc.emptySummary {
				assert.Contains(body, original[section])
			}
			for _, other := range []string{"summary", "notes", "transcript"} {
				if other != section {
					assert.Contains(body, "Updated "+other)
				}
			}
			delete(source.blockErrs, id)
			fresh := paragraph(id, "Recovered "+section, false)
			source.blocks[id] = &fresh
			if section == "transcript" {
				imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }
			}
			third, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			assert.False(third.PartialCoverage)
			if section == "transcript" {
				assert.Equal(int64(1), third.MaintenanceRetries)
			}
			assert.Empty(lastNotionState(t, st, third.SourceID).Pending)
			body, err = st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(err)
			assert.Contains(body, "Recovered "+section)
		})
	}
}

func TestDecodeArchivedEvidenceVersionProbe(t *testing.T) {
	for _, tc := range []struct {
		name, input  string
		newer, found bool
	}{
		{"version one", `{"schema_version":1,"discovery":{"id":"meeting-1","type":"meeting_notes"},"meeting_block":{"id":"meeting-1","type":"meeting_notes"},"page_markdown":{"object":"page_markdown","id":"page-1","markdown":"# Transcript\nSaved transcript"},"canonical":{"transcript":"Saved transcript"}}`, false, true},
		{"future incompatible", `{"schema_version":3,"summary":[]}`, true, false},
		{"missing", `{}`, false, false}, {"truncated", `{"schema_version":3`, false, false},
		{"current version missing discovery", `{"schema_version":1}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, found, err := decodeArchivedEvidence([]byte(tc.input), "meeting-1")
			assert.Equal(t, tc.found, found)
			var newer *NewerEvidenceError
			assert.Equal(t, tc.newer, errors.As(err, &newer))
		})
	}
}

func TestImporterNewerEvidencePreservesRetryState(t *testing.T) {
	for _, tc := range []struct {
		name    string
		now     time.Time
		expired bool
	}{
		{"within window", time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC), false},
		{"expired", time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, imp := newImporterFixture(t)
			second := hydrationMeeting()
			second.ID = "meeting-2"
			second.MeetingNotes.Children = MeetingChildren{SummaryBlockID: "summary-2", NotesBlockID: "notes-2", TranscriptBlockID: "transcript-2"}
			block := *source.blocks["meeting-1"]
			block.ID = second.ID
			var err error
			block.Raw, err = json.Marshal(second)
			require.NoError(err)
			source.blocks[second.ID] = &block
			for _, section := range []string{"summary", "notes", "transcript"} {
				value := paragraph(section+"-2", "Second "+section, false)
				source.blocks[section+"-2"] = &value
			}
			source.query.Results = append(source.query.Results, second)
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "# Notes\nNo transcript"
			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(err)
			before := lastNotionState(t, st, first.SourceID)
			var id int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&id))
			future := []byte(`{"schema_version":3,"summary":[],"page_markdown":"private text","transcript":42}`)
			require.NoError(st.UpsertMessageRawWithFormat(id, future, RawFormat))
			updated := paragraph("summary-2", "Changed second meeting", false)
			source.blocks["summary-2"] = &updated
			source.blockErrs = map[string]error{"summary-1": &APIError{Kind: ErrRateLimited, Status: 429}}
			imp.now = func() time.Time { return tc.now }
			_, err = imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			var newer *NewerEvidenceError
			require.ErrorAs(err, &newer)
			afterRaw, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.Equal(future, afterRaw)
			var secondID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-2'`).Scan(&secondID))
			body, err := st.GetMessageBodyText(t.Context(), secondID)
			require.NoError(err)
			assert.Contains(body, "Changed second meeting")
			checkpoint, err := st.GetLatestCheckpointedSyncByType(t.Context(), first.SourceID, SourceType)
			require.NoError(err)
			var after syncState
			require.NoError(json.Unmarshal([]byte(checkpoint.CursorBefore.String), &after))
			if tc.expired {
				assert.Empty(after.Pending)
			} else {
				assert.Equal(before.Pending, after.Pending)
			}
			assert.Equal(before.TranscriptRetryUntil, after.TranscriptRetryUntil)
		})
	}
}

func TestImporterRetriesOmittedTranscript(t *testing.T) {
	for _, tc := range []struct {
		name, markdown, wantBody string
		pending, truncated       bool
	}{
		{"page has transcript", "# Transcript\nNew page transcript", "New page transcript", false, false},
		{"page has none", "# Notes\nNo transcript", "Saved page transcript", true, false},
		{"markdown truncated", "# Notes\nTranscript heading was truncated", "Saved page transcript", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, source, imp := newImporterFixture(t)
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "# Transcript\nSaved page transcript"
			var err error
			source.markdown.Raw, err = json.Marshal(source.markdown)
			require.NoError(t, err)
			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(t, err)
			var id int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&id))
			before, err := st.GetMessageRaw(id)
			require.NoError(t, err)
			if !tc.truncated {
				source.query.Results[0].MeetingNotes.Children.TranscriptBlockID = ""
			}
			source.markdown.Markdown = tc.markdown
			source.markdown.Truncated = tc.truncated
			source.markdown.Raw, err = json.Marshal(source.markdown)
			require.NoError(t, err)
			second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(t, err)
			body, err := st.GetMessageBodyText(t.Context(), id)
			require.NoError(t, err)
			assert.Contains(t, body, tc.wantBody)
			pending := lastNotionState(t, st, first.SourceID).Pending
			if !tc.pending {
				assert.NotContains(t, body, "Saved page transcript")
				assert.Empty(t, pending)
				return
			}
			require.Len(t, pending, 1)
			assert.True(t, pending[0].Incomplete)
			assert.True(t, second.PartialCoverage)
			if tc.truncated {
				after, err := st.GetMessageRaw(id)
				require.NoError(t, err)
				var oldFields, newFields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(before, &oldFields))
				require.NoError(t, json.Unmarshal(after, &newFields))
				assert.JSONEq(t, string(oldFields["page_markdown"]), string(newFields["page_markdown"]))
				metadata, err := st.GetMessageMetadata(id)
				require.NoError(t, err)
				require.True(t, metadata.Valid)
				var fields map[string]any
				require.NoError(t, json.Unmarshal([]byte(metadata.String), &fields))
				assert.Equal(t, true, fields["markdown_transcript_fallback"])
			}
		})
	}
}

func TestImporterClearsCompletedEmptyTranscript(t *testing.T) {
	st, source, imp := newImporterFixture(t)
	first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(t, err)
	var id int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&id))
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Notes\nNo transcript"
	source.markdown.Raw, err = json.Marshal(source.markdown)
	require.NoError(t, err)
	second, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
	require.NoError(t, err)
	body, err := st.GetMessageBodyText(t.Context(), id)
	require.NoError(t, err)
	assert.NotContains(t, body, "Test Speaker: Ready to ship.")
	assert.False(t, second.PartialCoverage)
	pending := lastNotionState(t, st, first.SourceID).Pending
	require.Len(t, pending, 1)
	assert.False(t, pending[0].Incomplete)
}

func TestImporterRetriesIncompleteSectionsAcrossWindows(t *testing.T) {
	for _, tc := range []struct {
		name      string
		oldFirst  bool
		retries   []time.Time
		recoverAt time.Time
	}{
		{name: "first import after deadline", oldFirst: true, recoverAt: time.Date(2026, 9, 6, 18, 0, 0, 0, time.UTC)},
		{name: "repeated retries past deadline", retries: []time.Time{time.Date(2026, 9, 5, 10, 30, 0, 0, time.UTC), time.Date(2026, 9, 8, 10, 30, 0, 0, time.UTC)}, recoverAt: time.Date(2026, 9, 11, 10, 30, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, source, imp := newImporterFixture(t)
			if tc.oldFirst {
				imp.now = func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }
			} else {
				_, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
				require.NoError(t, err)
			}
			source.blockErrs = map[string]error{"summary-1": &APIError{Kind: ErrRateLimited, Status: 429}}
			first, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(t, err)
			pending := lastNotionState(t, st, first.SourceID).Pending
			require.Len(t, pending, 1)
			if tc.oldFirst {
				assert.True(t, pending[0].Incomplete)
			}
			source.query.Results = nil
			for _, now := range tc.retries {
				imp.now = func() time.Time { return now }
				result, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
				require.NoError(t, err)
				assert.Equal(t, int64(1), result.MaintenanceRetries)
				require.Len(t, lastNotionState(t, st, first.SourceID).Pending, 1)
			}
			delete(source.blockErrs, "summary-1")
			recovered := paragraph("summary-1", "Recovered summary", false)
			source.blocks["summary-1"] = &recovered
			imp.now = func() time.Time { return tc.recoverAt }
			result, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(t, err)
			assert.Equal(t, int64(1), result.MaintenanceRetries)
			assert.Empty(t, lastNotionState(t, st, first.SourceID).Pending)
			var id int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&id))
			body, err := st.GetMessageBodyText(t.Context(), id)
			require.NoError(t, err)
			assert.Contains(t, body, "Recovered summary")
		})
	}
}

func TestImporterRetainsActionsWhenReplacementSectionFails(t *testing.T) {
	for _, section := range []string{"summary", "notes"} {
		t.Run(section, func(t *testing.T) {
			st, source, imp := newImporterFixture(t)
			id := section + "-1"
			var task Block
			require.NoError(t, json.Unmarshal([]byte(`{"object":"block","id":"`+id+`","type":"to_do","has_children":false,"to_do":{"rich_text":[{"plain_text":"Saved action"}],"checked":false}}`), &task))
			source.blocks[id] = &task
			_, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(t, err)
			meeting := hydrationMeeting()
			meeting.LastEditedTime = "2026-08-29T12:30:00Z"
			if section == "summary" {
				meeting.MeetingNotes.Children.SummaryBlockID = "replacement"
			} else {
				meeting.MeetingNotes.Children.NotesBlockID = "replacement"
			}
			source.query.Results = []MeetingNote{meeting}
			source.blockErrs = map[string]error{"replacement": &APIError{Kind: ErrRateLimited, Status: 429}}
			result, err := imp.Import(t.Context(), ImportOptions{Identifier: "work"})
			require.NoError(t, err)
			assert.True(t, result.PartialCoverage)
			var messageID int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
			raw, err := st.GetMessageRaw(messageID)
			require.NoError(t, err)
			content := meetingcontent.Decode(RawFormat, raw, nil)
			assert.Equal(t, meetingcontent.CoveragePartial, content.ActionCoverage)
			require.Len(t, content.Actions, 1)
			assert.Equal(t, "Saved action", content.Actions[0].Title)
		})
	}
}
