package notionmeetings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"hegel.dev/go/hegel"
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

func (f *fakeImportSource) RetrieveSelf(context.Context) (*User, error) {
	return &User{Object: "user", ID: "bot-1", Type: "bot"}, nil
}
func (f *fakeImportSource) QueryMeetingNotesFiltered(ctx context.Context, limit int, filter *MeetingFilter) (*QueryResult, error) {
	result, err := f.QueryMeetingNotes(ctx, limit)
	if err != nil || result == nil || filter == nil {
		return result, err
	}
	filtered := *result
	filtered.Results = nil
	for _, meeting := range result.Results {
		at := parseNotionTime(meeting.CreatedTime)
		include := true
		for _, term := range filter.Filters {
			value := term.Filter.Value.Value
			bound, _ := time.Parse("2006-01-02 15:04", value.StartDate+" "+value.StartTime)
			if term.Filter.Operator == "date_is_before" && !at.Before(bound) {
				include = false
			}
			if term.Filter.Operator == "date_is_on_or_after" && at.Before(bound) {
				include = false
			}
		}
		if include {
			filtered.Results = append(filtered.Results, meeting)
		}
	}
	filtered.HasMore = result.HasMore && len(filtered.Results) > 0
	return &filtered, nil
}
func (f *fakeImportSource) Search(context.Context, string) (*DiscoveryPage, error) {
	return &DiscoveryPage{Object: "list"}, nil
}
func (f *fakeImportSource) QueryDataSource(context.Context, string, string) (*DiscoveryPage, error) {
	return &DiscoveryPage{Object: "list"}, nil
}
func (f *fakeImportSource) RetrieveDatabase(context.Context, string) (*Database, error) {
	return nil, ErrMalformedResponse
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

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1",
		Identifier: "work", AccountEmail: "user@example.com",
	})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	assert.Equal(int64(1), first.MeetingsProcessed)
	state := lastNotionState(t, st, first.SourceID)
	assert.Equal(syncStateVersion, state.Version)
	require.Contains(state.Known, "meeting-1")
	assert.Equal("2026-08-29T10:35:00Z", state.Known["meeting-1"].LastEditedTime)

	second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1",
		Identifier: "work", AccountEmail: "user@example.com",
	})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsProcessed)
	assert.Zero(second.MeetingsUpdated)

	meeting := hydrationMeeting()
	meeting.LastEditedTime = "2026-08-29T12:30:00Z"
	source.query.Results = []MeetingNote{meeting}
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	updated := paragraph("summary-1", "Ship the edited release scope.", false)
	source.blocks["summary-1"] = &updated
	third, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1",
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

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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

	repaired, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work", Full: true})
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

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind("DELETE FROM messages WHERE source_id = ?"), first.SourceID)
	require.NoError(err)

	second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
			first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)

			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
				SELECT id FROM messages
				WHERE source_id = ? AND source_message_id = 'meeting-1'
			`), first.SourceID).Scan(&messageID))
			tt.mutate(t, st, messageID)

			second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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

			first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), first.MeetingsAdded)

			tt.mutate(source)
			second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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

			third, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
	first, err := firstImporter.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	_, err = st.GetOrCreateSource(SourceType, "personal")
	require.NoError(err)
	secondImporter := NewImporter(st, source)
	secondImporter.now = firstImporter.now
	second, err := secondImporter.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "personal"})
	require.NoError(err)
	assert.NotEqual(first.SourceID, second.SourceID)

	var count int
	require.NoError(st.DB().QueryRowContext(
		t.Context(),
		`SELECT COUNT(*) FROM messages WHERE source_message_id = 'meeting-1'`,
	).Scan(&count))
	assert.Equal(2, count)
}

func TestImporterAfterFiltersNewMeetings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	otherStore, otherSource, otherImporter := newImporterFixture(t)
	otherSource.query.Results = []MeetingNote{hydrationMeeting()}
	after, err := otherImporter.Import(t.Context(), ImportOptions{UserID: "creator-1",
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

	summary, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(t, err)
	state := lastNotionState(t, st, summary.SourceID)
	assert.True(state.Coverage.HasMore)
	assert.GreaterOrEqual(state.Coverage.Returned, 1)
	assert.Positive(state.Coverage.Reasons["query_cap"])
	assert.True(summary.PartialCoverage)
}

func TestImporterHardFailureRetainsLastSuccessfulState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	before := lastNotionState(t, st, first.SourceID)

	meeting := hydrationMeeting()
	meeting.LastEditedTime = "2026-08-29T13:00:00Z"
	source.query.Results = []MeetingNote{meeting}
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	source.blockErrs = map[string]error{"meeting-1": ErrUnauthorized}
	failed, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.Error(err)
	assert.Equal(int64(1), failed.Errors)
	after, err := imp.loadState(t.Context(), first.SourceID)
	require.NoError(err)
	assert.Equal(before.Known, after.Known)
	assert.Equal(before.Pending, after.Pending)
	assert.Equal(before.TranscriptRetryUntil, after.TranscriptRetryUntil)
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

	summary, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.Error(err)
	assert.Equal(int64(1), summary.MeetingsAdded)
	assert.Equal(int64(1), summary.Errors)

	var messages int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&messages))
	assert.Equal(1, messages)
	cp, err := st.GetLatestCheckpointedSyncByType(t.Context(), summary.SourceID, SourceType)
	require.NoError(err)
	var pending syncState
	require.NoError(json.Unmarshal([]byte(cp.CursorBefore.String), &pending))
	assert.Contains(pending.Admissions, "meeting-1")
	assert.NotContains(pending.Known, "meeting-1")
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&messageID))
	if st.IsPostgreSQL() {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_notion_meeting_stats ON conversations; DROP FUNCTION fail_notion_meeting_stats()`)
	} else {
		_, err = st.DB().ExecContext(t.Context(), `DROP TRIGGER fail_notion_meeting_stats`)
	}
	require.NoError(err)
	replay, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	assert.Zero(replay.MeetingsAdded)
	var sameID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&sameID))
	assert.Equal(messageID, sameID)
	body, err := st.GetMessageBodyText(t.Context(), sameID)
	require.NoError(err)
	assert.Contains(body, "Decide the release scope.")
	recipients, err := st.GetMessageRecipientsContext(t.Context(), sameID, "to")
	require.NoError(err)
	var addresses []string
	for _, person := range recipients {
		addresses = append(addresses, person.EmailAddress)
	}
	assert.Contains(addresses, "attendee@example.com")
	assert.Contains(lastNotionState(t, st, summary.SourceID).Known, "meeting-1")
}

func TestImporterRetriesPendingTranscriptOutsideVisibleLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.blockErrs = map[string]error{
		"transcript-1": &APIError{Kind: ErrProvider, Status: 404, Code: "object_not_found"},
	}
	source.markdown.Markdown = "# Weekly planning"

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work", Limit: 1})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)
	state := lastNotionState(t, st, first.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("meeting-1", state.Pending[0].BlockID)
	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
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

	second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work", Limit: 1})
	require.NoError(err)
	assert.Equal(int64(1), second.MaintenanceRetries)
	state = lastNotionState(t, st, second.SourceID)
	assert.Empty(state.Pending)

	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "Transcript arrived.")
}

func TestImporterRetriesMissingStructuredTranscriptIgnoringParentMarkdown(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Partial transcript."
	source.markdown.Truncated = true
	source.markdown.Raw = json.RawMessage(`{"object":"page_markdown","id":"page-1","truncated":true}`)

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, first.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("meeting-1", state.Pending[0].BlockID)
	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
	assert.NotContains(body, "Partial transcript.")

	fullDiscovery, err := json.Marshal(hydrationMeeting())
	require.NoError(err)
	source.blocks["meeting-1"].Raw = fullDiscovery
	source.query.Results = nil
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Complete transcript."
	complete := paragraph("transcript-1", "Test Speaker: Complete transcript.", false)
	source.blocks["transcript-1"] = &complete
	source.markdown.Truncated = false
	source.markdown.Raw = json.RawMessage(`{"object":"page_markdown","id":"page-1","truncated":false}`)
	imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }

	second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.MaintenanceRetries)
	assert.Equal(int64(1), second.MeetingsUpdated)
	state = lastNotionState(t, st, second.SourceID)
	assert.Empty(state.Pending)
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
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
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning"

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, first.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("2026-08-31T12:00:00Z", state.Pending[0].RetryUntil)

	expiredImporter := NewImporter(st, source)
	expiredImporter.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	second, err := expiredImporter.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	state = lastNotionState(t, st, second.SourceID)
	assert.Empty(state.Pending)
	assert.Equal("2026-08-31T12:00:00Z", lastTranscriptRetryUntil(t, st, second.SourceID)["meeting-1"])

	muchLaterImporter := NewImporter(st, source)
	muchLaterImporter.now = func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }
	third, err := muchLaterImporter.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	state = lastNotionState(t, st, third.SourceID)
	assert.Empty(state.Pending)
	assert.Equal("2026-08-31T12:00:00Z", lastTranscriptRetryUntil(t, st, third.SourceID)["meeting-1"])

	updated := paragraph("summary-1", "Ship the post-expiry release scope.", false)
	source.blocks["summary-1"] = &updated
	editImporter := NewImporter(st, source)
	editImporter.now = func() time.Time { return time.Date(2026, 9, 2, 13, 0, 0, 0, time.UTC) }
	edited, err := editImporter.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), edited.MeetingsUpdated)
	state = lastNotionState(t, st, edited.SourceID)
	assert.Empty(state.Pending)
	var body string
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
	assert.Contains(body, "post-expiry release scope")

	late := paragraph("transcript-1", "Test Speaker: Transcript arrived after expiry.", false)
	source.blocks["transcript-1"] = &late
	source.markdown.Markdown = "# Weekly planning\n\n## Transcript\nTest Speaker: Transcript arrived after expiry."
	lateImporter := NewImporter(st, source)
	lateImporter.now = func() time.Time { return time.Date(2026, 9, 2, 14, 0, 0, 0, time.UTC) }
	lateSummary, err := lateImporter.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning"

	sourceRecord, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "work")
	require.NoError(err)
	syncID, err := st.StartSync(sourceRecord.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, `{"version":1,"last_query_at":"2026-08-29T12:00:00Z","known":{},"pending":[{"block_id":"meeting-1","next_attempt_at":"2026-08-29T18:00:00Z","retry_until":"2026-08-31T12:00:00Z"}],"coverage":{"has_more":false,"returned":1}}`))

	imp.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	summary, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning"

	_, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	expiredImporter := NewImporter(st, source)
	expiredImporter.now = func() time.Time { return time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC) }
	_, err = expiredImporter.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)

	meeting.MeetingNotes.CalendarEvent.EndTime = "2026-09-01T12:00:00Z"
	source.query.Results = []MeetingNote{meeting}
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	extendedImporter := NewImporter(st, source)
	extendedImporter.now = func() time.Time { return time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC) }
	extended, err := extendedImporter.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	state := lastNotionState(t, st, extended.SourceID)
	require.Len(state.Pending, 1)
	assert.Equal("2026-09-08T12:00:00Z", state.Pending[0].RetryUntil)
	assert.Equal("2026-09-08T12:00:00Z", lastTranscriptRetryUntil(t, st, extended.SourceID)["meeting-1"])
}

func TestSyncStateRejectsInvalidTimesWithSafeLocalDiagnostics(t *testing.T) {
	for _, field := range []string{"pending.next_attempt_at", "pending.retry_until", "transcript_retry_until"} {
		t.Run(field, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			const privateID = "PRIVATE_MEETING_ID"
			const privateTime = "PRIVATE_CURSOR_TIME https://example.test/?cursor=PRIVATE_CURSOR"
			const validTime = "2026-08-29T18:00:00Z"
			state := syncState{
				Version:              syncStateVersion,
				Pending:              []pendingTranscript{{BlockID: privateID, NextAttemptAt: validTime, RetryUntil: validTime}},
				TranscriptRetryUntil: map[string]string{privateID: validTime},
			}
			require.NoError(state.validate())
			switch field {
			case "pending.next_attempt_at":
				state.Pending[0].NextAttemptAt = privateTime
			case "pending.retry_until":
				state.Pending[0].RetryUntil = privateTime
			case "transcript_retry_until":
				state.TranscriptRetryUntil[privateID] = privateTime
			}
			err := state.validate()
			require.Error(err)
			assert.Equal(field+" must be an RFC3339 timestamp", err.Error())
			assert.NotContains(err.Error(), privateID)
			assert.NotContains(err.Error(), "PRIVATE_CURSOR")
			assert.NotContains(err.Error(), "content unavailable")
		})
	}
}

func TestImporterContinuesWhenUserListingFailsTransiently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	source.usersErr = errors.New("rate limit exceeded")

	summary, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
	assert.Contains(string(raw), "Notion User Information lookup failed; attendee emails were not resolved")
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

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
	second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), first.MeetingsAdded)

	meeting := hydrationMeeting()
	meeting.MeetingNotes.CalendarEvent.Attendees = []string{"user-1"}
	source.query.Results = []MeetingNote{meeting}
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	source.usersErr = errors.New("rate limit exceeded")
	second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
		_, err = imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
				installMeetingHeader(t, source.fakeHydrationSource, meeting)
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

			first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), first.MeetingsAdded)

			tt.mutate(source)
			second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	empty := paragraph("transcript-1", "", false)
	source.blocks["transcript-1"] = &empty
	source.markdown.Markdown = "# Weekly planning"

	first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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

	second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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

			first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)
			before := lastNotionState(t, st, first.SourceID)
			require.Len(before.Pending, 1)

			source.query.Results = nil
			source.blockErrs = map[string]error{"meeting-1": tt.err}
			imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }
			summary, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			if errors.Is(tt.err, ErrReadContent) || errors.Is(tt.err, ErrMalformedResponse) {
				require.NoError(err)
				assert.Positive(summary.CoverageReasons[errorCategory(tt.err)])
				return
			}
			require.Error(err)
			require.ErrorIs(err, tt.err)
			assert.Equal(int64(1), summary.Errors)

			after, err := imp.loadState(t.Context(), first.SourceID)
			require.NoError(err)
			assert.Equal(before.Known, after.Known)
			assert.Equal(before.Pending, after.Pending)
			assert.Equal(before.TranscriptRetryUntil, after.TranscriptRetryUntil)
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

	summary, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
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
				_, err := st.DB().ExecContext(t.Context(), st.Rebind("DELETE FROM message_raw WHERE message_id = ?"), messageID)
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
			st, source, imp := newImporterFixture(t)
			first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)

			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
				SELECT id FROM messages
				WHERE source_id = ? AND source_message_id = 'meeting-1'
			`), first.SourceID).Scan(&messageID))
			if tt.mutate != nil {
				tt.mutate(t, st, messageID)
			}

			meeting := hydrationMeeting()
			meeting.LastEditedTime = "2026-08-29T14:00:00Z"
			source.query.Results = []MeetingNote{meeting}
			installMeetingHeader(t, source.fakeHydrationSource, meeting)
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			source.markdown.Markdown = "# Weekly planning"
			second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)
			assert.True(second.PartialCoverage)
			if tt.mutate == nil {
				assert.Equal(int64(1), second.MeetingsUpdated)
			} else {
				assert.Zero(second.MeetingsUpdated)
			}

			var body string
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT body_text FROM message_bodies`).Scan(&body))
			assert.Contains(body, "Test Speaker: Ready to ship.")
			state := lastNotionState(t, st, first.SourceID)
			require.Len(state.Pending, 1)
		})
	}
}

func TestImporterRequiresRegisteredSource(t *testing.T) {
	st := testutil.NewTestStore(t)
	source := &fakeImportSource{fakeHydrationSource: completeHydrationSource(), query: &QueryResult{}}
	_, err := NewImporter(st, source).Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "removed"})
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

func TestLegacyUnscopedRefreshKeepsArchive(t *testing.T) {
	for _, bodyOnly := range []bool{false, true} {
		t.Run(strconv.FormatBool(bodyOnly), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st, source, imp := newImporterFixture(t)
			first, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)
			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
			raw, err := st.GetMessageRaw(messageID)
			require.NoError(err)
			var legacy map[string]any
			require.NoError(json.Unmarshal(raw, &legacy))
			legacy["schema_version"] = 1
			legacy["transcript"] = map[string]any{}
			legacy["page_markdown"] = map[string]any{"object": "page_markdown", "id": "page-1", "markdown": "## Transcript\nLEGACY_UNSCOPED_TRANSCRIPT"}
			canonical, ok := legacy["canonical"].(map[string]any)
			require.True(ok)
			canonical["transcript"] = "LEGACY_UNSCOPED_TRANSCRIPT"
			legacyRaw, err := json.Marshal(legacy)
			require.NoError(err)
			require.NoError(st.UpsertMessageRawWithFormat(messageID, legacyRaw, RawFormat))
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE message_bodies SET body_text=? WHERE message_id=?`), "Legacy body\n\nTranscript:\nLEGACY_UNSCOPED_TRANSCRIPT", messageID)
			require.NoError(err)
			if bodyOnly {
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM message_raw WHERE message_id=?`), messageID)
				require.NoError(err)
			}
			beforeBody, err := st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(err)
			beforeMeta, err := st.GetMessageMetadata(messageID)
			require.NoError(err)
			empty := paragraph("transcript-1", "", false)
			source.blocks["transcript-1"] = &empty
			beforeKnown := lastNotionState(t, st, first.SourceID).Known["meeting-1"]
			clock := imp.now().Add(time.Hour)
			imp.now = func() time.Time { return clock }
			var progress []string
			second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work", Progress: func(line string) { progress = append(progress, line) }})
			require.NoError(err)
			assert.Zero(second.MeetingsUpdated)
			afterKnown := lastNotionState(t, st, first.SourceID).Known["meeting-1"]
			assert.Equal(beforeKnown.LastSuccessAt, afterKnown.LastSuccessAt)
			assert.Equal(beforeKnown.SectionFreshness, afterKnown.SectionFreshness)
			assert.Equal("legacy_unscoped", afterKnown.FetchError)
			assert.True(second.PartialCoverage)
			assert.Contains(strings.Join(progress, "\n"), "Archived meeting retained unchanged")
			afterBody, err := st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(err)
			afterMeta, err := st.GetMessageMetadata(messageID)
			require.NoError(err)
			assert.Equal(beforeBody, afterBody)
			assert.Equal(beforeMeta, afterMeta)
			if !bodyOnly {
				afterRaw, err := st.GetMessageRaw(messageID)
				require.NoError(err)
				assert.Equal(checksumBytes(legacyRaw), checksumBytes(afterRaw))
			}
			assert.NotEmpty(lastNotionState(t, st, first.SourceID).Pending)
			fresh := paragraph("transcript-1", "FRESH_SCOPED_TRANSCRIPT", false)
			source.blocks[fresh.ID] = &fresh
			migrated, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)
			assert.Equal(int64(1), migrated.MeetingsUpdated)
			freshRaw, err := st.GetMessageRaw(messageID)
			require.NoError(err)
			assert.NotContains(string(freshRaw), "LEGACY_UNSCOPED_TRANSCRIPT")
			assert.NotContains(string(freshRaw), "page_markdown")
			content := meetingcontent.Decode(RawFormat, freshRaw, nil)
			assert.Equal("FRESH_SCOPED_TRANSCRIPT", content.Transcript.Text)
			state := lastNotionState(t, st, first.SourceID)
			assert.Empty(state.Pending)
		})
	}
}

func TestPartialSectionPreservesCompleteEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := newImporterFixture(t)
	_, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
	beforeRaw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	var before map[string]any
	require.NoError(json.Unmarshal(beforeRaw, &before))
	partial := paragraph("fresh-transcript", "Only the first part.", true)
	source.blocks[partial.ID] = &partial
	source.children[partial.ID] = map[string]*BlockPage{"": {HasMore: true, NextCursor: "next"}}
	source.childrenErrs = map[string]error{partial.ID + "/next": ErrRateLimited}
	header := hydrationMeeting()
	header.LastEditedTime = "2026-08-30T10:00:00Z"
	header.MeetingNotes.Children.TranscriptBlockID = partial.ID
	headerRaw, err := json.Marshal(header)
	require.NoError(err)
	require.NoError(json.Unmarshal(headerRaw, source.blocks["meeting-1"]))
	second, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), second.MeetingsUpdated)
	afterRaw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	var after map[string]any
	require.NoError(json.Unmarshal(afterRaw, &after))
	prior, ok := before["transcript"].(map[string]any)
	require.True(ok)
	retained, ok := after["transcript"].(map[string]any)
	require.True(ok)
	assert.Equal(prior["root"], retained["root"])
	assert.Equal(prior["blocks"], retained["blocks"])
	assert.Equal(prior["source_last_edited_time"], retained["source_last_edited_time"])
	assert.Equal(true, retained["preserved"])
	content := meetingcontent.Decode(RawFormat, afterRaw, nil)
	assert.Equal("Test Speaker: Ready to ship.", content.Transcript.Text)
	assert.NotContains(content.Transcript.Text, "Only the first part")
	body, err := st.GetMessageBodyText(t.Context(), messageID)
	require.NoError(err)
	assert.Contains(body, "Test Speaker: Ready to ship.")
	state := lastNotionState(t, st, second.SourceID)
	require.Len(state.Pending, 1, "preserved text does not finish the current incomplete fetch")
	assert.Equal("2026-08-29T18:00:00Z", state.Pending[0].NextAttemptAt)
	assert.Equal("2026-09-05T10:30:00Z", state.Pending[0].RetryUntil)
	source.query.Results = nil
	complete := paragraph(partial.ID, "The complete transcript.", false)
	source.blocks[complete.ID] = &complete
	imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }
	final, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	assert.Equal(int64(1), final.MaintenanceRetries)
	assert.Empty(lastNotionState(t, st, final.SourceID).Pending)
	finalRaw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.Equal("The complete transcript.", meetingcontent.Decode(RawFormat, finalRaw, nil).Transcript.Text)
}

func TestImporterRetriesNonemptyIncompleteTranscript(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, filtered := range []bool{false, true} {
			t.Run(fmt.Sprintf("legacy=%t/filtered=%t", legacy, filtered), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				st, source, imp := newImporterFixture(t)
				var legacyRaw []byte
				var messageID int64
				if legacy {
					_, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
					require.NoError(err)
					require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
					raw, err := st.GetMessageRaw(messageID)
					require.NoError(err)
					var evidence map[string]any
					require.NoError(json.Unmarshal(raw, &evidence))
					evidence["schema_version"] = 1
					evidence["page_markdown"] = map[string]any{"object": "page_markdown", "id": "page-1", "markdown": "legacy parent content"}
					legacyRaw, err = json.Marshal(evidence)
					require.NoError(err)
					require.NoError(st.UpsertMessageRawWithFormat(messageID, legacyRaw, RawFormat))
				}
				prefix := paragraph("transcript-1", "Only the first part.", true)
				source.blocks[prefix.ID] = &prefix
				if filtered {
					source.children[prefix.ID] = map[string]*BlockPage{"": {Results: []Block{*source.blocks["meeting-1"]}}}
				} else {
					source.children[prefix.ID] = map[string]*BlockPage{"": {HasMore: true, NextCursor: "next"}}
					source.childrenErrs = map[string]error{prefix.ID + "/next": ErrRateLimited}
				}
				partial, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
				require.NoError(err)
				state := lastNotionState(t, st, partial.SourceID)
				require.Len(state.Pending, 1, "nonempty partial or filtered content still needs a fresh complete fetch")
				assert.Equal("meeting-1", state.Pending[0].BlockID)
				assert.Equal("2026-08-29T18:00:00Z", state.Pending[0].NextAttemptAt)
				assert.Equal("2026-09-05T10:30:00Z", state.Pending[0].RetryUntil)
				assert.True(partial.PartialCoverage)
				if legacy {
					afterRaw, err := st.GetMessageRaw(messageID)
					require.NoError(err)
					assert.Equal(legacyRaw, afterRaw)
				} else {
					require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
				}
				// A retry before its cadence is due must leave the work pending.
				source.query.Results = nil
				imp.now = func() time.Time { return time.Date(2026, 8, 29, 17, 59, 0, 0, time.UTC) }
				early, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
				require.NoError(err)
				assert.Zero(early.MaintenanceRetries)
				assert.Equal(state.Pending, lastNotionState(t, st, partial.SourceID).Pending)
				complete := paragraph(prefix.ID, "The complete transcript.", false)
				source.blocks[complete.ID] = &complete
				imp.now = func() time.Time { return time.Date(2026, 8, 29, 18, 1, 0, 0, time.UTC) }
				final, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
				require.NoError(err)
				assert.Equal(int64(1), final.MaintenanceRetries)
				assert.Empty(lastNotionState(t, st, final.SourceID).Pending)
				finalRaw, err := st.GetMessageRaw(messageID)
				require.NoError(err)
				assert.Equal("The complete transcript.", meetingcontent.Decode(RawFormat, finalRaw, nil).Transcript.Text)
			})
		}
	}
}

func TestImporterIncompleteTranscriptHonorsRetryStopRules(t *testing.T) {
	for _, status := range []string{"failed", "deleted", "expired"} {
		t.Run(status, func(t *testing.T) {
			st, source, imp := newImporterFixture(t)
			meeting := hydrationMeeting()
			if status == "expired" {
				imp.now = func() time.Time { return time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) }
			} else {
				meeting.MeetingNotes.Status = status
				installMeetingHeader(t, source.fakeHydrationSource, meeting)
			}
			partial := paragraph("transcript-1", "Only the first part.", true)
			source.blocks[partial.ID] = &partial
			source.childrenErrs = map[string]error{partial.ID + "/": ErrRateLimited}
			result, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(t, err)
			assert.Equal(t, int64(1), result.MeetingsAdded)
			assert.Empty(t, lastNotionState(t, st, result.SourceID).Pending)
		})
	}
}

func TestArchivedV2RejectsUnprovenEvidence(t *testing.T) {
	hydrated, err := NewHydrator(completeHydrationSource()).Hydrate(t.Context(), hydrationMeeting())
	require.NoError(t, err)
	snapshot, err := hydrated.ArchiveSnapshot(1, "synthetic", "")
	require.NoError(t, err)
	for _, mutation := range []string{"wrong_root", "different_discovery", "wrong_canonical", "wrong_provenance", "unpreserved_timestamp", "unpreserved_source_header"} {
		t.Run(mutation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			var evidence map[string]any
			require.NoError(json.Unmarshal(snapshot.Raw, &evidence))
			tree, ok := evidence["transcript"].(map[string]any)
			require.True(ok)
			switch mutation {
			case "wrong_root":
				root, ok := tree["root"].(map[string]any)
				require.True(ok)
				root["id"] = "unrelated-section"
				blocks, ok := tree["blocks"].([]any)
				require.True(ok)
				require.NotEmpty(blocks)
				block, ok := blocks[0].(map[string]any)
				require.True(ok)
				block["id"] = "unrelated-section"
			case "different_discovery":
				discovery, ok := evidence["discovery"].(map[string]any)
				require.True(ok)
				discovery["last_edited_time"] = "1999-01-01T00:00:00Z"
			case "wrong_canonical":
				canonical, ok := evidence["canonical"].(map[string]any)
				require.True(ok)
				canonical["transcript"] = "UNATTRIBUTABLE"
			case "wrong_provenance":
				tree["preserved"] = true
				tree["source_meeting_block"] = evidence["meeting_block"]
				tree["source_last_edited_time"] = "1999-01-01T00:00:00Z"
			case "unpreserved_timestamp":
				tree["source_last_edited_time"] = "1999-01-01T00:00:00Z"
			case "unpreserved_source_header":
				unrelated := hydrationMeeting()
				unrelated.ID = "unrelated-meeting"
				tree["source_meeting_block"] = unrelated
			}
			raw, err := json.Marshal(evidence)
			require.NoError(err)
			_, ok = decodeArchivedEvidence(raw, "meeting-1")
			assert.False(ok, "unproven evidence must retain the entire prior snapshot on an incomplete refresh")
			decoded := meetingcontent.Decode(RawFormat, raw, nil)
			freshProvenanceMutation := strings.HasPrefix(mutation, "unpreserved_")
			if mutation == "wrong_canonical" || freshProvenanceMutation {
				assert.Equal("Test Speaker: Ready to ship.", decoded.Transcript.Text)
			} else {
				assert.Empty(decoded.Transcript.Text)
			}
			st, source, imp := newImporterFixture(t)
			_, err = imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)
			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
			require.NoError(st.UpsertMessageRawWithFormat(messageID, raw, RawFormat))
			beforeBody, err := st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(err)
			beforeMeta, err := st.GetMessageMetadata(messageID)
			require.NoError(err)
			empty := paragraph("transcript-1", "", false)
			source.blocks[empty.ID] = &empty
			result, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(err)
			assert.True(result.PartialCoverage)
			afterRaw, err := st.GetMessageRaw(messageID)
			require.NoError(err)
			assert.Equal(checksumBytes(raw), checksumBytes(afterRaw))
			if freshProvenanceMutation {
				after := meetingcontent.Decode(RawFormat, afterRaw, nil)
				assert.Equal("Test Speaker: Ready to ship.", after.Transcript.Text)
				assert.NotEqual("invalid_section", after.Transcript.Reason, "retention must not emit an unreadable preserved section")
			}
			afterBody, err := st.GetMessageBodyText(t.Context(), messageID)
			require.NoError(err)
			assert.Equal(beforeBody, afterBody)
			afterMeta, err := st.GetMessageMetadata(messageID)
			require.NoError(err)
			assert.Equal(beforeMeta, afterMeta)
		})
	}
}

func TestImporterDiagnosticsDoNotExposeMeetingIdentifiers(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			assert := assert.New(t)

			_, source, imp := newImporterFixture(t)
			if fail {
				source.blockErrs = map[string]error{"meeting-1": errors.New("https://example.test/PRIVATE_ID?cursor=PRIVATE_CURSOR")}
			}
			var progress []string
			_, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work", Progress: func(line string) { progress = append(progress, line) }})
			output := strings.Join(progress, "\n")
			require.NoError(t, err)
			assert.NotContains(output, "meeting-1")
			assert.NotContains(output, "PRIVATE_ID")
			assert.NotContains(output, "PRIVATE_CURSOR")
		})
	}
}

func TestImporterIncompleteSectionsPreserveActionEvidence(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		filtered := hegel.Draw(ht, hegel.Booleans())
		text := "Send brief " + hegel.Draw(ht, hegel.Text())
		st, source, imp := newImporterFixture(t)
		root := paragraph("notes-1", "", true)
		source.blocks[root.ID] = &root
		taskRaw, err := json.Marshal(map[string]any{"object": "block", "id": "owned-task", "type": "to_do", "has_children": false, "to_do": map[string]any{"rich_text": []RichText{{PlainText: text}}, "checked": false}})
		require.NoError(ht, err)
		var task Block
		require.NoError(ht, json.Unmarshal(taskRaw, &task))
		source.children[root.ID][""].Results = []Block{task}
		_, err = imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
		require.NoError(ht, err)
		var messageID int64
		require.NoError(ht, st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
		beforeRaw, err := st.GetMessageRaw(messageID)
		require.NoError(ht, err)
		var before map[string]any
		require.NoError(ht, json.Unmarshal(beforeRaw, &before))
		beforeNotes, ok := before["notes"].(map[string]any)
		require.True(ht, ok)
		replacement := paragraph("replacement-text", text, false)
		page := &BlockPage{Results: []Block{replacement}}
		if filtered {
			var nested Block
			require.NoError(ht, json.Unmarshal([]byte(`{"object":"block","id":"foreign","type":"meeting_notes","has_children":false,"meeting_notes":{}}`), &nested))
			page.Results = append(page.Results, nested)
		} else {
			page.HasMore = true
			page.NextCursor = "missing-tail"
			source.childrenErrs = map[string]error{"notes-1/missing-tail": ErrRateLimited}
		}
		for index, edit := range []string{"2026-08-30T10:00:00Z", "2026-08-31T10:00:00Z"} {
			freshRoot := paragraph(fmt.Sprintf("fresh-notes-%d", index), "", true)
			source.blocks[freshRoot.ID] = &freshRoot
			source.children[freshRoot.ID] = map[string]*BlockPage{"": page}
			if !filtered {
				source.childrenErrs[freshRoot.ID+"/missing-tail"] = ErrRateLimited
			}
			header := hydrationMeeting()
			header.LastEditedTime = edit
			header.MeetingNotes.Children.NotesBlockID = freshRoot.ID
			installMeetingHeader(t, source.fakeHydrationSource, header)
			_, err = imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
			require.NoError(ht, err)
			raw, err := st.GetMessageRaw(messageID)
			require.NoError(ht, err)
			var current map[string]any
			require.NoError(ht, json.Unmarshal(raw, &current))
			notes, ok := current["notes"].(map[string]any)
			require.True(ht, ok)
			assert.Equal(ht, beforeNotes["root"], notes["root"])
			assert.Equal(ht, beforeNotes["blocks"], notes["blocks"])
			assert.Equal(ht, beforeNotes["source_last_edited_time"], notes["source_last_edited_time"])
			assert.Equal(ht, before["meeting_block"], notes["source_meeting_block"])
			content := meetingcontent.Decode(RawFormat, raw, nil)
			require.Len(ht, content.Actions, 1)
			assert.Equal(ht, "owned-task", content.Actions[0].SourceID)
			assert.Equal(ht, meetingcontent.StatusPending, content.Actions[0].Status)
		}
		// A complete, unfiltered fetch can update the action even when text is equal.
		var taskFields map[string]any
		require.NoError(ht, json.Unmarshal(taskRaw, &taskFields))
		taskFields["id"] = "fresh-complete-task"
		todo, ok := taskFields["to_do"].(map[string]any)
		require.True(ht, ok)
		todo["checked"] = true
		updatedRaw, err := json.Marshal(taskFields)
		require.NoError(ht, err)
		var updated Block
		require.NoError(ht, json.Unmarshal(updatedRaw, &updated))
		source.blocks[updated.ID] = &updated
		header := hydrationMeeting()
		header.MeetingNotes.Children.NotesBlockID = updated.ID
		header.LastEditedTime = "2026-09-01T10:00:00Z"
		installMeetingHeader(t, source.fakeHydrationSource, header)
		_, err = imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
		require.NoError(ht, err)
		raw, err := st.GetMessageRaw(messageID)
		require.NoError(ht, err)
		content := meetingcontent.Decode(RawFormat, raw, nil)
		require.Len(ht, content.Actions, 1)
		assert.Equal(ht, "fresh-complete-task", content.Actions[0].SourceID)
		assert.Equal(ht, meetingcontent.StatusCompleted, content.Actions[0].Status)
		var final map[string]any
		require.NoError(ht, json.Unmarshal(raw, &final))
		assert.NotContains(ht, final["notes"], "preserved")
	}, hegel.WithTestCases(16), hegel.WithDatabase(""))
}

func TestImporterPreservedSnapshotStabilizes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := newImporterFixture(t)
	_, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	empty := paragraph("transcript-1", "", false)
	source.blocks[empty.ID] = &empty
	_, err = imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
	before, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	imp.now = func() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) }
	result, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work"})
	require.NoError(err)
	assert.Zero(result.MeetingsUpdated)
	after, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.Equal(before, after)
}

func TestImporterAcceptsEmptyCompleteSummaryAndNotes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := newImporterFixture(t)
	noSearch := false
	_, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work", SearchAccessible: &noSearch})
	require.NoError(err)
	for _, id := range []string{"summary-1", "notes-1"} {
		empty := paragraph(id, "", false)
		source.blocks[id] = &empty
	}
	meeting := hydrationMeeting()
	meeting.LastEditedTime = "2026-08-30T10:00:00Z"
	source.query.Results = []MeetingNote{meeting}
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	result, err := imp.Import(t.Context(), ImportOptions{UserID: "creator-1", Identifier: "work", Full: true, SearchAccessible: &noSearch})
	require.NoError(err)
	assert.False(result.PartialCoverage)
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	content := meetingcontent.Decode(RawFormat, raw, nil)
	assert.Empty(content.Summary.Text)
	assert.Empty(content.Notes.Text)
}

func TestImporterReportsPreservedSectionWhenHeaderOmitsID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st, source, imp := newImporterFixture(t)
	noSearch := false
	opts := ImportOptions{UserID: "creator-1", Identifier: "work", SearchAccessible: &noSearch}
	first, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	meeting := hydrationMeeting()
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	var header map[string]any
	require.NoError(json.Unmarshal(source.blocks[meeting.ID].Raw, &header))
	payload, ok := header["meeting_notes"].(map[string]any)
	require.True(ok)
	children, ok := payload["children"].(map[string]any)
	require.True(ok)
	delete(children, "summary_block_id")
	updated, err := json.Marshal(header)
	require.NoError(err)
	require.NoError(json.Unmarshal(updated, source.blocks[meeting.ID]))
	source.query.Results = []MeetingNote{meeting}
	opts.Full = true
	result, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	state, err := imp.loadState(t.Context(), first.SourceID)
	require.NoError(err)
	assert.Equal("incomplete_sections", state.Known[meeting.ID].FetchError)
	assert.True(result.PartialCoverage)
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.NotEmpty(meetingcontent.Decode(RawFormat, raw, nil).Summary.Text)
}

func TestImporterPreservesNonTextEvidenceOnIncompleteRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	var image Block
	require.NoError(json.Unmarshal([]byte(`{"object":"block","id":"notes-1","type":"image","has_children":false,"image":{"type":"external","external":{"url":"https://example.com/image.png"}}}`), &image))
	source.blocks["notes-1"] = &image
	noSearch := false
	opts := ImportOptions{Identifier: "work", UserID: "creator-1", SearchAccessible: &noSearch}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
	meeting := hydrationMeeting()
	meeting.LastEditedTime = "2026-08-30T10:00:00Z"
	source.query.Results = []MeetingNote{meeting}
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	source.blockErrs = map[string]error{"notes-1": ErrRateLimited}
	opts.Full = true
	result, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.True(result.PartialCoverage)
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	var evidence rawEvidence
	require.NoError(json.Unmarshal(raw, &evidence))
	assert.True(evidence.Notes.Preserved)
	assert.Contains(string(evidence.Notes.Root), "image.png")
}

func TestImporterClearsExplicitlyRemovedSections(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, imp := newImporterFixture(t)
	noSearch := false
	opts := ImportOptions{Identifier: "work", UserID: "creator-1", SearchAccessible: &noSearch}
	_, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	meeting := hydrationMeeting()
	meeting.LastEditedTime = "2026-08-30T10:00:00Z"
	meeting.MeetingNotes.Children.SummaryBlockID = ""
	meeting.MeetingNotes.Children.NotesBlockID = ""
	source.query.Results = []MeetingNote{meeting}
	installMeetingHeader(t, source.fakeHydrationSource, meeting)
	opts.Full = true
	result, err := imp.Import(t.Context(), opts)
	require.NoError(err)
	assert.False(result.PartialCoverage)
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages WHERE source_message_id='meeting-1'`).Scan(&messageID))
	raw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	content := meetingcontent.Decode(RawFormat, raw, nil)
	assert.Empty(content.Summary.Text)
	assert.Empty(content.Notes.Text)
	assert.Empty(content.Actions)
}
