package circleback

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// fakeSource is an in-memory meetingSource with per-call failure switches.
type fakeSource struct {
	meetings           map[string]json.RawMessage // id -> meeting JSON
	searchMeetings     map[string]json.RawMessage // optional search-only summary JSON
	transcripts        map[string]json.RawMessage // id -> transcript JSON
	searchErr          error
	readErr            error
	transcriptErr      error
	failRead           bool
	failTranscripts    bool
	omitRead           map[string]bool
	orderedIDs         []string
	respectSearchStart bool
	searchPages        [][]string
	searchedPages      []int
	searchedIDs        []string
	readIDs            []string
	transcriptIDs      []string
	searchCalls        int
	searchHook         func(int)
	readHook           func([]string)
	transcriptHook     func([]string)
	operations         []string

	lastSearchStart string
}

func (f *fakeSource) SearchMeetings(_ context.Context, start, _ string, pageIndex int) ([]Meeting, error) {
	f.lastSearchStart = start
	f.searchCalls++
	f.operations = append(f.operations, "search")
	if f.searchHook != nil {
		f.searchHook(pageIndex)
	}
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	var out []Meeting
	ids := f.orderedIDs
	if f.searchPages != nil {
		f.searchedPages = append(f.searchedPages, pageIndex)
		if pageIndex >= len(f.searchPages) {
			return nil, nil
		}
		ids = f.searchPages[pageIndex]
	} else if pageIndex > 0 {
		return nil, nil
	}
	if len(ids) == 0 {
		if f.searchPages != nil {
			return nil, nil
		}
		ids = make([]string, 0, len(f.meetings))
		for id := range f.meetings {
			ids = append(ids, id)
		}
	}
	var searchStart time.Time
	if start != "" {
		searchStart = parseFlexibleTime(start)
	}
	for _, id := range ids {
		raw := f.meetings[id]
		if summary, ok := f.searchMeetings[id]; ok {
			raw = summary
		}
		var m Meeting
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		m.Raw = raw
		if f.respectSearchStart && !searchStart.IsZero() && m.ScheduledAt().Before(searchStart) {
			continue
		}
		out = append(out, m)
		f.searchedIDs = append(f.searchedIDs, id)
	}
	return out, nil
}

func (f *fakeSource) ReadMeetings(_ context.Context, ids []string) ([]Meeting, error) {
	f.readIDs = append(f.readIDs, ids...)
	f.operations = append(f.operations, "read:"+strings.Join(ids, ","))
	if f.readHook != nil {
		f.readHook(ids)
	}
	if f.readErr != nil {
		return nil, f.readErr
	}
	if f.failRead {
		return nil, context.DeadlineExceeded
	}
	var out []Meeting
	for _, id := range ids {
		if f.omitRead[id] {
			continue
		}
		raw, ok := f.meetings[id]
		if !ok {
			continue
		}
		var m Meeting
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		m.Raw = raw
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeSource) GetTranscripts(_ context.Context, ids []string) (map[string]*Transcript, error) {
	f.transcriptIDs = append(f.transcriptIDs, ids...)
	f.operations = append(f.operations, "transcript:"+strings.Join(ids, ","))
	if f.transcriptHook != nil {
		f.transcriptHook(ids)
	}
	if f.transcriptErr != nil {
		return nil, f.transcriptErr
	}
	if f.failTranscripts {
		return nil, context.DeadlineExceeded
	}
	out := map[string]*Transcript{}
	for _, id := range ids {
		raw, ok := f.transcripts[id]
		if !ok {
			continue
		}
		var tr Transcript
		if err := json.Unmarshal(raw, &tr); err != nil {
			return nil, err
		}
		tr.Raw = raw
		out[id] = &tr
	}
	return out, nil
}

const meeting42 = `{
	"id": 42,
	"name": "Design Review",
	"createdAt": "2026-06-10T17:05:00Z",
	"startTime": "2026-06-10T17:00:00Z",
	"endTime": "2026-06-10T17:45:00Z",
	"durationSeconds": 2700,
	"organizer": {"name": "Alice Smith", "email": "alice@example.com"},
	"attendees": [
		{"name": "Alice Smith", "email": "alice@example.com"},
		{"name": "Bob Jones", "email": "bob@example.com"},
		{"name": "Guest Speaker"}
	],
	"notes": "## Decisions\n- Ship the new layout",
	"actionItems": [
		{"title": "Update mockups", "assignee": "Bob Jones", "status": "pending"}
	],
	"tags": ["design"],
	"meetingUrl": "https://meet.example.com/design",
	"recordingUrl": "https://cdn.example.com/rec/42.mp4"
}`

const transcript42 = `{
	"meetingId": 42,
	"transcript": [
		{"speaker": "Alice Smith", "text": "Welcome to the design review.", "start": 0},
		{"speaker": "Bob Jones", "text": "The mockups are ready.", "start": 65}
	]
}`

const plainTranscript42 = `{
	"id": 42,
	"text": "Plain archive key remains searchable after the refresh."
}`

const refreshedMeeting42 = `{
	"id": 42,
	"name": "Design Review Refreshed",
	"createdAt": "2026-06-12T09:00:00Z",
	"startTime": "2026-06-10T17:00:00Z",
	"endTime": "2026-06-10T18:00:00Z",
	"durationSeconds": 3600,
	"organizer": {"name": "Alice Smith", "email": "alice@example.com"},
	"attendees": [
		{"name": "Alice Smith", "email": "alice@example.com"},
		{"name": "Carol Jones", "email": "carol@example.com"}
	],
	"notes": "Refreshedsignal notes from the current meeting payload",
	"actionItems": [{"description": "Reviewbudgetdelta before launch"}],
	"insights": [{"title": "Forecast", "content": "Pineapplemetric is improving"}],
	"tags": ["refreshed", "forecasttag"]
}`

type archivedTranscriptFixture struct {
	name     string
	payload  json.RawMessage
	bodyText string
	ftsTerm  string
}

var archivedTranscriptFixtures = []archivedTranscriptFixture{
	{
		name:     "structured archive",
		payload:  json.RawMessage(transcript42),
		bodyText: "[00:00] Alice Smith: Welcome to the design review.",
		ftsTerm:  "welcome",
	},
	{
		name:     "plain text archive",
		payload:  json.RawMessage(plainTranscript42),
		bodyText: "Plain archive key remains searchable after the refresh.",
		ftsTerm:  "archive",
	},
}

type transcriptRefreshFixture struct {
	name    string
	payload json.RawMessage
}

var transcriptRefreshFixtures = []transcriptRefreshFixture{
	{name: "omitted transcript id"},
	{name: "empty transcript list", payload: json.RawMessage(`{"id": 42, "transcript": []}`)},
	{name: "blank text", payload: json.RawMessage(`{"id": 42, "text": "  \n\t"}`)},
}

func newTestImporter(t *testing.T, f *fakeSource) (*Importer, *store.Store) {
	t.Helper()
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, f)
	imp.now = func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }
	return imp, st
}

func meetingFixture(t *testing.T, id, createdAt, startTime, endTime string) json.RawMessage {
	t.Helper()
	payload := map[string]any{
		"id":        id,
		"name":      "Meeting " + id,
		"createdAt": createdAt,
		"startTime": startTime,
		"endTime":   endTime,
		"notes":     "Notes for " + id,
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return raw
}

func TestImport_AccountIdentityControlsFromMe(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	meetingFor := func(id, organizerEmail string) json.RawMessage {
		var payload map[string]any
		require.NoError(json.Unmarshal(meetingFixture(
			t, id, "2026-07-09T10:30:00Z", "2026-07-09T10:00:00Z", "2026-07-09T11:00:00Z",
		), &payload))
		payload["organizer"] = map[string]any{"name": "Test Organizer", "email": organizerEmail}
		raw, err := json.Marshal(payload)
		require.NoError(err)
		return raw
	}
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"primary": meetingFor("primary", "USER-A@EXAMPLE.COM"),
			"alias":   meetingFor("alias", "user-b@example.com"),
			"other":   meetingFor("other", "user-c@example.com"),
		},
		transcripts: map[string]json.RawMessage{},
		orderedIDs:  []string{"primary", "alias", "other"},
	}
	imp, st := newTestImporter(t, f)
	source, err := st.GetOrCreateSource(SourceType, "work")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(source.ID, " User-B@Example.COM ", "manual"))

	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier:   "work",
		AccountEmail: " user-a@example.com ",
	})
	require.NoError(err)
	assert.Equal(source.ID, sum.SourceID)

	for _, tc := range []struct {
		id   string
		want bool
	}{
		{id: "primary", want: true},
		{id: "alias", want: true},
		{id: "other", want: false},
	} {
		var got bool
		require.NoError(st.DB().QueryRow(st.Rebind(
			`SELECT is_from_me FROM messages WHERE source_id = ? AND source_message_id = ?`),
			source.ID, "meeting:"+tc.id).Scan(&got))
		assert.Equal(tc.want, got, tc.id)
	}

	msgID := circlebackMessageIDFor(t, st, "primary")
	assert.Equal("work", circlebackMetadataMap(t, st, msgID)["account_identifier"],
		"metadata preserves the source label")
}

func TestImport_NormalizesSentAtToUTC(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := &fakeSource{meetings: map[string]json.RawMessage{
		"offset": meetingFixture(t, "offset", "2026-07-09T19:30:00Z",
			"2026-07-09T15:00:00-05:00", "2026-07-09T16:00:00-05:00"),
	}}
	imp, st := newTestImporter(t, f)

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	var sentAt string
	require.NoError(st.DB().QueryRow(`SELECT CAST(sent_at AS TEXT) FROM messages`).Scan(&sentAt))
	assert.Contains(sentAt, "2026-07-09 20:00:00")
	assert.NotContains(sentAt, "-05:00")
}

func seedCirclebackState(t *testing.T, st *store.Store, state syncState) *store.SyncRun {
	t.Helper()
	return seedCirclebackCursor(t, st, state.marshal())
}

func seedCirclebackCursor(t *testing.T, st *store.Store, cursor string) *store.SyncRun {
	t.Helper()
	src, err := st.GetOrCreateSource(SourceType, "alice@example.com")
	require.NoError(t, err)
	syncID, err := st.StartSync(src.ID, SourceType)
	require.NoError(t, err)
	require.NoError(t, st.CompleteSync(syncID, cursor))
	run, err := st.GetLastSuccessfulSync(src.ID)
	require.NoError(t, err)
	return run
}

func lastCirclebackState(t *testing.T, st *store.Store, sourceID int64) syncState {
	t.Helper()
	run, err := st.GetLastSuccessfulSync(sourceID)
	require.NoError(t, err)
	require.True(t, run.CursorAfter.Valid)
	var state syncState
	require.NoError(t, json.Unmarshal([]byte(run.CursorAfter.String), &state))
	return state
}

func pendingTranscriptByID(t *testing.T, state syncState, meetingID string) pendingTranscript {
	t.Helper()
	for _, pending := range state.PendingTranscripts {
		if pending.MeetingID == meetingID {
			return pending
		}
	}
	require.FailNow(t, "pending transcript not found", "meeting id %q", meetingID)
	return pendingTranscript{}
}

func countLoggedSQLStatements(t *testing.T, logs *bytes.Buffer, fragment string) int {
	t.Helper()
	count := 0
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		statement, _ := record["stmt"].(string)
		if record["msg"] == "sql" && strings.Contains(statement, fragment) {
			count++
		}
	}
	return count
}

func circlebackMessageIDFor(t *testing.T, st *store.Store, meetingID string) int64 {
	t.Helper()
	var msgID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`), "meeting:"+meetingID).Scan(&msgID))
	return msgID
}

func TestRetrySchedule(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		meeting     Meeting
		previous    *pendingTranscript
		wantPending bool
		wantNext    string
		wantUntil   string
	}{
		{
			name: "recent past meeting uses six-hour cadence and seven-day deadline",
			meeting: Meeting{
				ID:        "recent",
				StartTime: "2026-07-08T10:00:00Z",
			},
			wantPending: true,
			wantNext:    "2026-07-09T18:00:00Z",
			wantUntil:   "2026-07-15T10:00:00Z",
		},
		{
			name: "future meeting first retries at parsed end",
			meeting: Meeting{
				ID:        "future-end",
				StartTime: "2026-07-10T10:00:00Z",
				EndTime:   "2026-07-10T11:00:00Z",
			},
			wantPending: true,
			wantNext:    "2026-07-10T11:00:00Z",
			wantUntil:   "2026-07-17T10:00:00Z",
		},
		{
			name: "future meeting without end first retries one hour after start",
			meeting: Meeting{
				ID:        "future-no-end",
				StartTime: "2026-07-10T10:00:00Z",
			},
			wantPending: true,
			wantNext:    "2026-07-10T11:00:00Z",
			wantUntil:   "2026-07-17T10:00:00Z",
		},
		{
			name: "created time alone does not anchor retry lifecycle",
			meeting: Meeting{
				ID:        "unknown",
				CreatedAt: "2020-01-01T00:00:00Z",
				StartTime: "not-a-time",
				Date:      "also-not-a-time",
			},
			wantPending: true,
			wantNext:    "2026-07-09T18:00:00Z",
			wantUntil:   "2026-07-11T12:00:00Z",
		},
		{
			name: "repeated omission retains deadline and advances cadence",
			meeting: Meeting{
				ID:        "repeat",
				StartTime: "2026-07-03T12:00:00Z",
			},
			previous: &pendingTranscript{
				MeetingID:     "repeat",
				NextAttemptAt: "2026-07-09T12:00:00Z",
				RetryUntil:    "2026-07-10T12:00:00Z",
			},
			wantPending: true,
			wantNext:    "2026-07-09T18:00:00Z",
			wantUntil:   "2026-07-10T12:00:00Z",
		},
		{
			name: "postponed meeting extends retry deadline",
			meeting: Meeting{
				ID:        "postponed",
				StartTime: "2026-07-20T10:00:00Z",
			},
			previous: &pendingTranscript{
				MeetingID:     "postponed",
				NextAttemptAt: "2026-07-09T12:00:00Z",
				RetryUntil:    "2026-07-10T12:00:00Z",
			},
			wantPending: true,
			wantNext:    "2026-07-09T18:00:00Z",
			wantUntil:   "2026-07-27T10:00:00Z",
		},
		{
			name: "deadline schedules a terminal maintenance transition",
			meeting: Meeting{
				ID:        "near-deadline",
				StartTime: "2026-07-02T15:00:00Z",
			},
			previous: &pendingTranscript{
				MeetingID:     "near-deadline",
				NextAttemptAt: "2026-07-09T12:00:00Z",
				RetryUntil:    "2026-07-09T15:00:00Z",
			},
			wantPending: true,
			wantNext:    "2026-07-09T15:00:00Z",
			wantUntil:   "2026-07-09T15:00:00Z",
		},
		{
			name: "expired meeting becomes unavailable",
			meeting: Meeting{
				ID:        "expired",
				StartTime: "2026-07-01T10:00:00Z",
			},
			wantPending: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			got, pending := schedulePendingTranscript(&tt.meeting, now, tt.previous)

			assert.Equal(tt.wantPending, pending)
			if !tt.wantPending {
				return
			}
			assert.Equal(string(tt.meeting.ID), got.MeetingID)
			assert.Equal(tt.wantNext, got.NextAttemptAt)
			assert.Equal(tt.wantUntil, got.RetryUntil)
		})
	}
}

func TestPendingTranscript_MissingOrRecognizedEmptyPersistsAndAdvancesWatermark(t *testing.T) {
	tests := []struct {
		name       string
		transcript json.RawMessage
	}{
		{name: "missing result"},
		{name: "recognized empty result", transcript: json.RawMessage(`{"id":"recent","transcript":[]}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			meeting := meetingFixture(t, "recent", "2026-07-09T10:30:00Z", "2026-07-09T10:00:00Z", "2026-07-09T11:00:00Z")
			f := &fakeSource{
				meetings:    map[string]json.RawMessage{"recent": meeting},
				transcripts: map[string]json.RawMessage{},
				orderedIDs:  []string{"recent"},
			}
			if tt.transcript != nil {
				f.transcripts["recent"] = tt.transcript
			}
			imp, st := newTestImporter(t, f)

			sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

			require.NoError(err)
			assert.EqualValues(1, sum.MeetingsProcessed)
			assert.EqualValues(1, sum.MeetingsAdded)
			assert.Zero(sum.Errors)
			state := lastCirclebackState(t, st, sum.SourceID)
			assert.Equal("2026-07-09T10:30:00Z", state.CreatedAfter)
			require.Len(state.PendingTranscripts, 1)
			pending := state.PendingTranscripts[0]
			assert.Equal("recent", pending.MeetingID)
			assert.Equal("2026-07-09T18:00:00Z", pending.NextAttemptAt)
			assert.Equal("2026-07-16T10:00:00Z", pending.RetryUntil)
			msgID := circlebackMessageIDFor(t, st, "recent")
			assert.Equal("pending", circlebackMetadataMap(t, st, msgID)["transcript_state"])
			assert.Contains(circlebackMessageBody(t, st, msgID), "Notes for recent")
		})
	}
}

func TestPendingTranscript_SearchOverlapSuppressesNotDueMeeting(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	prior := syncState{
		CreatedAfter: "2026-07-09T11:00:00Z",
		PendingTranscripts: []pendingTranscript{{
			MeetingID:     "42",
			NextAttemptAt: "2026-07-09T13:00:00Z",
			RetryUntil:    "2026-07-16T10:00:00Z",
		}},
	}
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
		orderedIDs:  []string{"42"},
	}
	imp, st := newTestImporter(t, f)
	seeded := seedCirclebackState(t, st, prior)

	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier:   "alice@example.com",
		AccountEmail: "alice@example.com",
	})

	require.NoError(err)
	assert.EqualValues(0, sum.MeetingsProcessed)
	assert.Equal([]string{"42"}, f.searchedIDs)
	assert.Empty(f.readIDs)
	assert.Empty(f.transcriptIDs)
	assert.Equal(prior, lastCirclebackState(t, st, seeded.SourceID))
}

func TestPendingTranscript_DueMeetingFetchedDirectlyAndDeduplicated(t *testing.T) {
	for _, searched := range []bool{false, true} {
		t.Run(fmt.Sprintf("searched=%t", searched), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			meeting := meetingFixture(t, "due", "2026-07-01T10:00:00Z", "2026-07-09T10:00:00Z", "2026-07-09T11:00:00Z")
			pages := [][]string{{}}
			if searched {
				pages = [][]string{{"due"}, {}}
			}
			f := &fakeSource{
				meetings:    map[string]json.RawMessage{"due": meeting},
				transcripts: map[string]json.RawMessage{"due": json.RawMessage(`{"id":"due","text":"Transcript ready"}`)},
				searchPages: pages,
			}
			imp, st := newTestImporter(t, f)
			prior := syncState{PendingTranscripts: []pendingTranscript{{
				MeetingID:     "due",
				NextAttemptAt: "2026-07-09T11:00:00Z",
				RetryUntil:    "2026-07-16T10:00:00Z",
			}}}
			seeded := seedCirclebackState(t, st, prior)

			sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com", Limit: 1})

			require.NoError(err)
			assert.EqualValues(1, sum.MeetingsProcessed)
			assert.EqualValues(1, sum.MaintenanceRetries)
			assert.Equal([]string{"due"}, f.readIDs)
			assert.Equal([]string{"due"}, f.transcriptIDs)
			assert.Empty(lastCirclebackState(t, st, seeded.SourceID).PendingTranscripts)
			msgID := circlebackMessageIDFor(t, st, "due")
			assert.Equal("present", circlebackMetadataMap(t, st, msgID)["transcript_state"])
		})
	}
}

func TestPendingTranscript_MaintenanceRunsBeforeCancelableSearchBacklog(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dueIDs := []string{"due-1", "due-2", "due-3", "due-4", "due-5"}
	meetings := map[string]json.RawMessage{
		"new-search": meetingFixture(t, "new-search", "2026-07-09T11:00:00Z", "2026-07-09T10:00:00Z", ""),
	}
	transcripts := make(map[string]json.RawMessage, len(dueIDs))
	pending := make([]pendingTranscript, 0, len(dueIDs))
	for i, id := range dueIDs {
		meetings[id] = meetingFixture(t, id,
			fmt.Sprintf("2026-07-09T%02d:00:00Z", 5+i),
			fmt.Sprintf("2026-07-09T%02d:00:00Z", 5+i), "")
		transcripts[id] = json.RawMessage(fmt.Sprintf(`{"id":%q,"text":"ready"}`, id))
		pending = append(pending, pendingTranscript{
			MeetingID:     id,
			NextAttemptAt: "2026-07-09T11:00:00Z",
			RetryUntil:    "2026-07-16T12:00:00Z",
		})
	}
	f := &fakeSource{
		meetings:    meetings,
		transcripts: transcripts,
		orderedIDs:  []string{"new-search"},
	}
	imp, st := newTestImporter(t, f)
	seeded := seedCirclebackState(t, st, syncState{PendingTranscripts: pending})
	ctx, cancel := context.WithCancel(context.Background())
	f.readHook = func(ids []string) {
		if countString(ids, "new-search") > 0 {
			cancel()
		}
	}

	sum, err := imp.Import(ctx, ImportOptions{Identifier: "alice@example.com"})

	require.ErrorIs(err, context.Canceled)
	require.NotNil(sum)
	require.GreaterOrEqual(len(f.readIDs), len(dueIDs)+1)
	assert.Equal(dueIDs, f.readIDs[:len(dueIDs)], "due maintenance must fill the first batch")
	assert.Equal("new-search", f.readIDs[len(dueIDs)], "searched backlog follows due maintenance")
	assert.Equal(dueIDs, f.transcriptIDs, "searched cancellation must occur only after due work is attempted")
	latest, latestErr := st.GetLatestSync(seeded.SourceID)
	require.NoError(latestErr)
	assert.Equal(store.SyncStatusFailed, latest.Status)
}

func TestPendingTranscript_MaintenanceRunsBeforeSearchFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"due-before-search": meetingFixture(t, "due-before-search",
				"2026-07-09T10:00:00Z", "2026-07-09T09:00:00Z", ""),
			"never-searched": meetingFixture(t, "never-searched",
				"2026-07-09T11:00:00Z", "2026-07-09T10:00:00Z", ""),
		},
		transcripts: map[string]json.RawMessage{
			"due-before-search": json.RawMessage(`{"id":"due-before-search","text":"ready before search"}`),
		},
		searchErr: context.DeadlineExceeded,
	}
	imp, st := newTestImporter(t, f)
	priorState := syncState{
		CreatedAfter: "2026-07-08T00:00:00Z",
		PendingTranscripts: []pendingTranscript{{
			MeetingID:     "due-before-search",
			NextAttemptAt: "2026-07-09T11:00:00Z",
			RetryUntil:    "2026-07-16T09:00:00Z",
		}},
	}
	prior := seedCirclebackState(t, st, priorState)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.Error(err)
	assert.Equal([]string{
		"read:due-before-search",
		"transcript:due-before-search",
		"search",
	}, f.operations)
	assert.EqualValues(1, sum.MeetingsProcessed)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.EqualValues(1, sum.Errors, "the later search operation is the only error")
	dueMsgID := circlebackMessageIDFor(t, st, "due-before-search")
	assert.Equal("present", circlebackMetadataMap(t, st, dueMsgID)["transcript_state"])
	var searchedCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`),
		"meeting:never-searched").Scan(&searchedCount))
	assert.Zero(searchedCount)
	latest, latestErr := st.GetLatestSync(prior.SourceID)
	require.NoError(latestErr)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	lastSuccessful, successErr := st.GetLastSuccessfulSync(prior.SourceID)
	require.NoError(successErr)
	assert.Equal(prior.ID, lastSuccessful.ID)
	assert.Equal(prior.CursorAfter, lastSuccessful.CursorAfter,
		"item-atomic due writes must be safely revisited from the prior successful cursor")
}

func TestPendingTranscript_FailedRunRecoversPersistedRetryBeforeCreationCutoff(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	oldID := "old-pending"
	meetings := map[string]json.RawMessage{
		oldID: meetingFixture(t, oldID,
			"2026-07-01T10:00:00Z", "2026-07-09T10:00:00Z", "2026-07-09T11:00:00Z"),
	}
	orderedIDs := []string{oldID}
	for i := 1; i < readBatchSize; i++ {
		id := fmt.Sprintf("current-%d", i)
		meetings[id] = meetingFixture(t, id,
			fmt.Sprintf("2026-07-09T%02d:00:00Z", i), "2026-07-09T10:00:00Z", "")
		orderedIDs = append(orderedIDs, id)
	}
	hardFailureID := "missing-detail"
	meetings[hardFailureID] = meetingFixture(t, hardFailureID,
		"2026-07-09T11:30:00Z", "2026-07-09T11:00:00Z", "")
	orderedIDs = append(orderedIDs, hardFailureID)

	f := &fakeSource{
		meetings:    meetings,
		transcripts: map[string]json.RawMessage{},
		omitRead:    map[string]bool{hardFailureID: true},
		orderedIDs:  orderedIDs,
	}
	imp, st := newTestImporter(t, f)
	priorState := syncState{CreatedAfter: "2026-07-09T11:00:00Z"}
	prior := seedCirclebackState(t, st, priorState)

	var archived Meeting
	require.NoError(json.Unmarshal(meetings[oldID], &archived))
	archived.Raw = meetings[oldID]
	added, changed, err := imp.ingestMeeting(
		prior.SourceID, "alice@example.com", nil, &archived, nil, transcriptStateUnavailable, false,
	)
	require.NoError(err)
	assert.True(added)
	assert.True(changed)

	failed, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com",
		Full:       true,
	})
	require.Error(err)
	assert.EqualValues(readBatchSize, failed.MeetingsProcessed)
	msgID := circlebackMessageIDFor(t, st, oldID)
	assert.Equal("pending", circlebackMetadataMap(t, st, msgID)["transcript_state"])
	assert.Equal(priorState, lastCirclebackState(t, st, prior.SourceID),
		"the later hard failure must retain the prior successful cursor")

	f.orderedIDs = []string{oldID}
	f.readIDs = nil
	f.transcriptIDs = nil
	f.transcripts[oldID] = json.RawMessage(`{"id":"old-pending","text":"Recovered after the failed run"}`)
	var sqlLogs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&sqlLogs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true})
	t.Cleanup(func() {
		store.ConfigureSQLLogging(store.SQLLogOptions{})
		slog.SetDefault(previousLogger)
	})

	retried, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(1, retried.MeetingsProcessed)
	assert.Equal([]string{oldID}, f.readIDs)
	assert.Equal([]string{oldID}, f.transcriptIDs)
	assert.Equal(1, countLoggedSQLStatements(t, &sqlLogs, "SELECT source_message_id, id, metadata"),
		"the search page should batch archive metadata")
	assert.Equal(2, countLoggedSQLStatements(t, &sqlLogs, "SELECT metadata FROM messages WHERE id"),
		"only hydration and persistence, not discovery filtering, should read per-message metadata")
	assert.Equal("present", circlebackMetadataMap(t, st, msgID)["transcript_state"])
}

func TestPendingTranscript_TerminalExpiryRunsBeforeSearchFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"expired-before-search": meetingFixture(t, "expired-before-search",
				"2026-07-02T10:00:00Z", "2026-07-02T09:00:00Z", ""),
		},
		transcripts: map[string]json.RawMessage{
			"expired-before-search": json.RawMessage(`{"id":"expired-before-search","text":"too late"}`),
		},
		searchErr: context.DeadlineExceeded,
	}
	imp, st := newTestImporter(t, f)
	prior := seedCirclebackState(t, st, syncState{PendingTranscripts: []pendingTranscript{{
		MeetingID:     "expired-before-search",
		NextAttemptAt: "2026-07-09T12:00:00Z",
		RetryUntil:    "2026-07-09T12:00:00Z",
	}}})

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.Error(err)
	assert.Equal([]string{"read:expired-before-search", "search"}, f.operations,
		"terminal expiry is persisted before search without another transcript call")
	assert.Empty(f.transcriptIDs)
	msgID := circlebackMessageIDFor(t, st, "expired-before-search")
	assert.Equal("unavailable", circlebackMetadataMap(t, st, msgID)["transcript_state"])
	lastSuccessful, successErr := st.GetLastSuccessfulSync(prior.SourceID)
	require.NoError(successErr)
	assert.Equal(prior.CursorAfter, lastSuccessful.CursorAfter)
}

func TestImport_MidTranscriptCancellationStopsBeforePersistence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"cancel-transcript": meetingFixture(t, "cancel-transcript",
				"2026-07-09T11:00:00Z", "2026-07-09T10:00:00Z", ""),
		},
		transcripts: map[string]json.RawMessage{},
		orderedIDs:  []string{"cancel-transcript"},
	}
	imp, st := newTestImporter(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	f.transcriptHook = func([]string) { cancel() }

	sum, err := imp.Import(ctx, ImportOptions{Identifier: "alice@example.com"})

	require.ErrorIs(err, context.Canceled)
	require.NotNil(sum)
	assert.EqualValues(1, sum.Errors)
	assert.Equal([]string{"cancel-transcript"}, f.transcriptIDs)
	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`),
		"meeting:cancel-transcript").Scan(&messageCount))
	assert.Zero(messageCount, "cancellation during transcript fetch must not persist notes-only data")
	latest, latestErr := st.GetLatestSync(sum.SourceID)
	require.NoError(latestErr)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.EqualValues(1, latest.ErrorsCount)
}

func TestImport_TranscriptCancellationErrorStopsBeforePersistence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"cancel-error": meetingFixture(t, "cancel-error",
				"2026-07-09T11:00:00Z", "2026-07-09T10:00:00Z", ""),
		},
		transcripts:   map[string]json.RawMessage{},
		transcriptErr: context.Canceled,
		orderedIDs:    []string{"cancel-error"},
	}
	imp, st := newTestImporter(t, f)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.ErrorIs(err, context.Canceled)
	assert.EqualValues(1, sum.Errors)
	var messageCount int
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_message_id = ?`),
		"meeting:cancel-error").Scan(&messageCount))
	assert.Zero(messageCount, "a canceled transcript call must never fall back to notes-only persistence")
}

func TestImport_MalformedPendingCursorFailsRecordedSync(t *testing.T) {
	tests := []struct {
		name   string
		cursor string
	}{
		{name: "malformed JSON", cursor: `{"created_after":`},
		{name: "invalid creation watermark", cursor: `{"created_after":"not-a-time"}`},
		{name: "blank meeting ID", cursor: `{"created_after":"","pending_transcripts":[{"meeting_id":" ","next_attempt_at":"2026-07-09T12:00:00Z","retry_until":"2026-07-10T12:00:00Z"}]}`},
		{name: "duplicate meeting ID", cursor: `{"created_after":"","pending_transcripts":[{"meeting_id":"duplicate","next_attempt_at":"2026-07-09T12:00:00Z","retry_until":"2026-07-10T12:00:00Z"},{"meeting_id":"duplicate","next_attempt_at":"2026-07-09T13:00:00Z","retry_until":"2026-07-10T12:00:00Z"}]}`},
		{name: "invalid next attempt", cursor: `{"created_after":"","pending_transcripts":[{"meeting_id":"pending","next_attempt_at":"bad","retry_until":"2026-07-10T12:00:00Z"}]}`},
		{name: "invalid deadline", cursor: `{"created_after":"","pending_transcripts":[{"meeting_id":"pending","next_attempt_at":"2026-07-09T12:00:00Z","retry_until":"bad"}]}`},
		{name: "next attempt after deadline", cursor: `{"created_after":"","pending_transcripts":[{"meeting_id":"pending","next_attempt_at":"2026-07-11T12:00:00Z","retry_until":"2026-07-10T12:00:00Z"}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := &fakeSource{meetings: map[string]json.RawMessage{}, transcripts: map[string]json.RawMessage{}}
			imp, st := newTestImporter(t, f)
			prior := seedCirclebackCursor(t, st, tt.cursor)

			sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

			require.Error(err)
			require.NotNil(sum)
			assert.Contains(err.Error(), "cursor")
			assert.Zero(f.searchCalls, "invalid cursor must fail before provider traversal")
			latest, latestErr := st.GetLatestSync(prior.SourceID)
			require.NoError(latestErr)
			assert.Equal(store.SyncStatusFailed, latest.Status)
			assert.EqualValues(1, latest.ErrorsCount)
			lastSuccessful, successErr := st.GetLastSuccessfulSync(prior.SourceID)
			require.NoError(successErr)
			assert.Equal(prior.ID, lastSuccessful.ID)
			assert.Equal(prior.CursorAfter, lastSuccessful.CursorAfter)
		})
	}
}

func TestImport_FutureMeetingWithoutCreatedAtDoesNotAdvanceCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const meetingID = "future-without-created-at"
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			meetingID: meetingFixture(t, meetingID, "", "2026-08-01T10:00:00Z", "2026-08-01T11:00:00Z"),
		},
		transcripts: map[string]json.RawMessage{
			meetingID: json.RawMessage(`{"id":"future-without-created-at","text":"Scheduled later"}`),
		},
		orderedIDs: []string{meetingID},
	}
	imp, st := newTestImporter(t, f)
	priorState := syncState{CreatedAfter: "2026-07-09T11:00:00Z"}
	prior := seedCirclebackState(t, st, priorState)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.Equal(priorState.CreatedAfter, lastCirclebackState(t, st, prior.SourceID).CreatedAfter,
		"a scheduled start is not a provider creation timestamp")
}

func TestImport_TranscriptBatchFailureCountsAffectedMeetings(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"failed-1": meetingFixture(t, "failed-1", "2026-07-09T09:00:00Z", "2026-07-09T09:00:00Z", ""),
			"failed-2": meetingFixture(t, "failed-2", "2026-07-09T10:00:00Z", "2026-07-09T10:00:00Z", ""),
		},
		transcripts:   map[string]json.RawMessage{},
		transcriptErr: context.DeadlineExceeded,
		orderedIDs:    []string{"failed-1", "failed-2"},
	}
	imp, st := newTestImporter(t, f)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.Error(err)
	assert.EqualValues(2, sum.Errors)
	latest, latestErr := st.GetLatestSync(sum.SourceID)
	require.NoError(latestErr)
	assert.EqualValues(2, latest.ErrorsCount)
}

func TestImport_SearchFailureCountsOneOperation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{},
		transcripts: map[string]json.RawMessage{},
		searchErr:   context.DeadlineExceeded,
	}
	imp, st := newTestImporter(t, f)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

	require.Error(err)
	assert.EqualValues(1, sum.Errors)
	latest, latestErr := st.GetLatestSync(sum.SourceID)
	require.NoError(latestErr)
	assert.EqualValues(1, latest.ErrorsCount)
}

func TestPendingTranscript_RepeatedOmissionExpiresAndFullPromotes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	meeting := meetingFixture(t, "lifecycle", "2026-07-03T18:00:00Z", "2026-07-03T18:00:00Z", "2026-07-03T19:00:00Z")
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"lifecycle": meeting},
		transcripts: map[string]json.RawMessage{},
		searchPages: [][]string{{}},
	}
	imp, st := newTestImporter(t, f)
	prior := syncState{PendingTranscripts: []pendingTranscript{{
		MeetingID:     "lifecycle",
		NextAttemptAt: "2026-07-09T12:00:00Z",
		RetryUntil:    "2026-07-10T18:00:00Z",
	}}}
	seeded := seedCirclebackState(t, st, prior)

	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	rescheduled := pendingTranscriptByID(t, lastCirclebackState(t, st, seeded.SourceID), "lifecycle")
	assert.Equal("2026-07-09T18:00:00Z", rescheduled.NextAttemptAt)
	assert.Equal("2026-07-10T18:00:00Z", rescheduled.RetryUntil)
	msgID := circlebackMessageIDFor(t, st, "lifecycle")
	assert.Equal("pending", circlebackMetadataMap(t, st, msgID)["transcript_state"])

	imp.now = func() time.Time { return time.Date(2026, 7, 10, 18, 0, 0, 0, time.UTC) }
	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.Empty(lastCirclebackState(t, st, seeded.SourceID).PendingTranscripts)
	assert.Equal("unavailable", circlebackMetadataMap(t, st, msgID)["transcript_state"])

	f.searchPages = nil
	f.orderedIDs = []string{"lifecycle"}
	f.transcripts["lifecycle"] = json.RawMessage(`{"id":"lifecycle","text":"Available after the bounded retry window"}`)
	f.meetings["lifecycle"] = json.RawMessage(`{
		"id":"lifecycle",
		"name":"Lifecycle refreshed",
		"createdAt":"2026-07-10T19:00:00Z",
		"startTime":"2026-07-03T18:00:00Z",
		"notes":"Refreshed notes after transcript expiry"
	}`)
	f.transcriptIDs = nil
	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.Equal("unavailable", circlebackMetadataMap(t, st, msgID)["transcript_state"],
		"ordinary incremental overlap must not restart or promote an expired retry lifecycle")
	assert.Empty(f.transcriptIDs, "unavailable transcripts are retried only by --full")
	assert.Contains(circlebackMessageBody(t, st, msgID), "Refreshed notes after transcript expiry",
		"transcript suppression must not block ordinary meeting-detail refreshes")

	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com", Full: true})
	require.NoError(err)
	assert.Equal("present", circlebackMetadataMap(t, st, msgID)["transcript_state"])
}

func TestImport_LimitCapsOnlyNewSearchWork(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ids := []string{"due-1", "new-1", "due-2", "new-2"}
	meetings := make(map[string]json.RawMessage, len(ids))
	for i, id := range ids {
		meetings[id] = meetingFixture(t, id,
			fmt.Sprintf("2026-07-09T%02d:00:00Z", 8+i),
			fmt.Sprintf("2026-07-09T%02d:00:00Z", 8+i), "")
	}
	f := &fakeSource{
		meetings:    meetings,
		transcripts: map[string]json.RawMessage{},
		orderedIDs:  ids,
	}
	imp, st := newTestImporter(t, f)
	prior := syncState{
		CreatedAfter: "2026-07-08T00:00:00Z",
		PendingTranscripts: []pendingTranscript{
			{MeetingID: "due-1", NextAttemptAt: "2026-07-09T11:00:00Z", RetryUntil: "2026-07-16T08:00:00Z"},
			{MeetingID: "due-2", NextAttemptAt: "2026-07-09T11:00:00Z", RetryUntil: "2026-07-16T10:00:00Z"},
		},
	}
	seeded := seedCirclebackState(t, st, prior)
	var progress []string

	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com",
		Limit:      1,
		Progress:   func(line string) { progress = append(progress, line) },
	})

	require.NoError(err)
	assert.EqualValues(3, sum.MeetingsProcessed, "one searched ID plus two due maintenance IDs")
	assert.EqualValues(2, sum.MaintenanceRetries)
	assert.ElementsMatch([]string{"new-1", "due-1", "due-2"}, f.readIDs)
	assert.NotContains(f.readIDs, "new-2")
	for _, id := range []string{"new-1", "due-1", "due-2"} {
		assert.Equal(1, countString(f.readIDs, id), "worklist ID %s must be deduplicated", id)
	}
	state := lastCirclebackState(t, st, seeded.SourceID)
	assert.Equal(prior.CreatedAfter, state.CreatedAfter, "limited traversal holds the creation watermark")
	assert.Len(state.PendingTranscripts, 3)
	assert.Contains(strings.Join(progress, "\n"), "2 due transcript maintenance items")
}

func TestImport_FullLimitPreservesUnprocessedPending(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	meetings := map[string]json.RawMessage{
		"pending-selected": meetingFixture(t, "pending-selected", "2026-07-09T09:00:00Z", "2026-07-09T09:00:00Z", ""),
		"pending-held":     meetingFixture(t, "pending-held", "2026-07-09T10:00:00Z", "2026-07-09T10:00:00Z", ""),
		"pending-due":      meetingFixture(t, "pending-due", "2026-07-09T08:00:00Z", "2026-07-09T08:00:00Z", ""),
	}
	f := &fakeSource{
		meetings: meetings,
		transcripts: map[string]json.RawMessage{
			"pending-selected": json.RawMessage(`{"id":"pending-selected","text":"Full sync found it"}`),
		},
		searchPages: [][]string{{"pending-selected", "pending-held"}, {}},
	}
	imp, st := newTestImporter(t, f)
	held := pendingTranscript{MeetingID: "pending-held", NextAttemptAt: "2026-07-10T12:00:00Z", RetryUntil: "2026-07-16T10:00:00Z"}
	prior := syncState{
		CreatedAfter: "2026-07-08T00:00:00Z",
		PendingTranscripts: []pendingTranscript{
			{MeetingID: "pending-selected", NextAttemptAt: "2026-07-10T12:00:00Z", RetryUntil: "2026-07-16T09:00:00Z"},
			held,
			{MeetingID: "pending-due", NextAttemptAt: "2026-07-09T11:00:00Z", RetryUntil: "2026-07-16T08:00:00Z"},
		},
	}
	seeded := seedCirclebackState(t, st, prior)

	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier: "alice@example.com",
		Full:       true,
		Limit:      1,
	})

	require.NoError(err)
	assert.EqualValues(2, sum.MeetingsProcessed, "one full-search selection plus one due maintenance retry")
	assert.EqualValues(1, sum.MaintenanceRetries)
	assert.ElementsMatch([]string{"pending-selected", "pending-due"}, f.readIDs)
	assert.NotContains(f.readIDs, "pending-held")
	state := lastCirclebackState(t, st, seeded.SourceID)
	assert.Equal(prior.CreatedAfter, state.CreatedAfter)
	assert.Len(state.PendingTranscripts, 2)
	assert.Equal(held, pendingTranscriptByID(t, state, "pending-held"),
		"pending outside the limited full worklist must remain unchanged")
	assert.Equal("2026-07-09T18:00:00Z",
		pendingTranscriptByID(t, state, "pending-due").NextAttemptAt)
	for _, pending := range state.PendingTranscripts {
		assert.NotEqual("pending-selected", pending.MeetingID,
			"full sync must bypass not-due suppression for the selected pending ID")
	}
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func TestImport_RoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
	}
	imp, st := newTestImporter(t, f)

	sum, err := imp.Import(context.Background(), ImportOptions{
		Identifier:   "alice@example.com",
		AccountEmail: "alice@example.com",
	})
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsProcessed)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.EqualValues(0, sum.Errors)

	// Message row.
	var subject, sentAt string
	var fromMe bool
	var hasAttachments bool
	var attachmentCount int
	var msgID int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id, subject, sent_at, is_from_me, has_attachments, attachment_count
		 FROM messages WHERE source_message_id = ?`),
		"meeting:42").Scan(&msgID, &subject, &sentAt, &fromMe, &hasAttachments, &attachmentCount))
	assert.Equal("Design Review", subject)
	assert.Contains(sentAt, "2026-06-10")
	assert.True(fromMe, "organizer alice IS the account identifier")
	assert.False(hasAttachments, "expiring recording URLs must not become durable attachments")
	assert.Equal(0, attachmentCount)

	// Conversation.
	var convType string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT c.conversation_type FROM conversations c
		JOIN messages m ON m.conversation_id = c.id WHERE m.id = ?`), msgID).Scan(&convType))
	assert.Equal("meeting", convType)

	// Body: notes, action items, transcript lines; name-only guest in the
	// body but not as a participant.
	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT body_text FROM message_bodies WHERE message_id = ?`), msgID).Scan(&body))
	assert.Contains(body, "Ship the new layout")
	assert.Contains(body, "- Update mockups (Bob Jones) [pending]")
	assert.Contains(body, "[00:00] Alice Smith: Welcome to the design review.")
	assert.Contains(body, "[01:05] Bob Jones: The mockups are ready.")
	assert.Contains(body, "Attendees: Alice Smith, Bob Jones, Guest Speaker")

	var toCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`),
		msgID).Scan(&toCount))
	assert.Equal(2, toCount, "name-only attendee must not become a recipient")

	// The short-lived recording URL remains in provider metadata but is not
	// represented as a durable attachment.
	var recordingAttachments int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments WHERE message_id = ?`), msgID).Scan(&recordingAttachments))
	assert.Zero(recordingAttachments)

	// Metadata.
	metaNS, err := st.GetMessageMetadata(msgID)
	require.NoError(err)
	require.True(metaNS.Valid)
	var meta meetingMetadata
	require.NoError(json.Unmarshal([]byte(metaNS.String), &meta))
	assert.Equal("circleback", meta.Platform)
	assert.Equal("42", meta.MeetingID)
	assert.EqualValues(2700, meta.DurationSeconds)
	assert.Equal("https://meet.example.com/design", meta.MeetingURL)
	assert.Equal("https://cdn.example.com/rec/42.mp4", meta.RecordingURL)
	assert.Equal("2026-07-09T12:00:00Z", meta.RecordingFetchedAt)
	require.Len(meta.ActionItems, 1)
	assert.Equal("Update mockups", meta.ActionItems[0].Title)
	assert.Equal([]string{"design"}, meta.Tags)
	assert.Equal(2, meta.TranscriptSegments)

	// Raw archive composes both verbatim payloads.
	raw, err := st.GetMessageRaw(msgID)
	require.NoError(err)
	var composed map[string]json.RawMessage
	require.NoError(json.Unmarshal(raw, &composed))
	assert.JSONEq(meeting42, string(composed["meeting"]))
	assert.JSONEq(transcript42, string(composed["transcript"]))
}

func TestImport_IdempotentRefreshAndWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
	}
	imp, st := newTestImporter(t, f)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	require.EqualValues(1, sum.MeetingsAdded)
	assert.Empty(f.lastSearchStart, "first run has no watermark")

	cursorOf := func() string {
		var blob string
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT cursor_after FROM sync_runs
			WHERE source_id = ? AND status = 'completed'
			ORDER BY id DESC LIMIT 1`), sum.SourceID).Scan(&blob))
		var state syncState
		require.NoError(json.Unmarshal([]byte(blob), &state))
		return state.CreatedAfter
	}
	require.Equal("2026-06-10T17:05:00Z", cursorOf())

	// Second run: discovery remains unbounded, but an identical overlap row is
	// neither added nor counted as an update.
	sum2, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(0, sum2.MeetingsAdded)
	assert.EqualValues(0, sum2.MeetingsUpdated, "identical overlap rows are no-op refreshes")
	assert.Empty(f.lastSearchStart, "incremental discovery is not bounded by scheduled date")

	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(1, count)

	// Failing run: watermark holds.
	f.failRead = true
	sum3, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.Error(err)
	assert.Positive(sum3.Errors)
	assert.Equal("2026-06-10T17:05:00Z", cursorOf(), "cursor must hold after a failing run")
	f.failRead = false

	// Server-side edit refreshes in place.
	edited := map[string]json.RawMessage{
		"42": json.RawMessage(`{"id": 42, "name": "Design Review v2", "createdAt": "2026-06-12T09:00:00Z", "startTime": "2026-06-10T17:00:00Z", "organizer": {"name": "Alice Smith", "email": "alice@example.com"}, "attendees": [{"name": "Alice Smith", "email": "alice@example.com"}], "notes": "updated"}`),
	}
	f.meetings = edited
	sum4, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(0, sum4.MeetingsAdded)
	assert.EqualValues(1, sum4.MeetingsUpdated)
	latest, err := st.GetLatestSync(sum4.SourceID)
	require.NoError(err)
	assert.Equal(sum4.MeetingsUpdated, latest.MessagesUpdated, "refresh count persisted to sync history")

	var subject string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT subject FROM messages WHERE source_message_id = ?`), "meeting:42").Scan(&subject))
	assert.Equal("Design Review v2", subject)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(1, count, "refresh must update in place")
	assert.Equal("2026-06-12T09:00:00Z", cursorOf(), "watermark advances to the edited createdAt")
	var conversationParticipantCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN messages m ON m.conversation_id = cp.conversation_id
		WHERE m.source_message_id = ?`), "meeting:42").Scan(&conversationParticipantCount))
	assert.Equal(1, conversationParticipantCount, "refresh must remove stale conversation participants")

	// The recording link attachment was removed on refresh (edited meeting
	// has no recordingUrl).
	var attCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments a JOIN messages m ON m.id = a.message_id
		WHERE m.source_message_id = ?`), "meeting:42").Scan(&attCount))
	assert.Equal(0, attCount, "stale recording link cleared by unconditional replace")
	var hasAttachments bool
	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT has_attachments, attachment_count FROM messages
		WHERE source_message_id = ?`), "meeting:42").Scan(&hasAttachments, &attachmentCount))
	assert.False(hasAttachments, "removing recording link must update message attachment metadata")
	assert.Equal(0, attachmentCount)
}

func TestImport_IncrementalDiscoversBackfilledMeetingBeforeScheduledOverlap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"current": meetingFixture(t, "current", "2026-06-10T17:05:00Z", "2026-06-10T17:00:00Z", "2026-06-10T18:00:00Z"),
		},
		transcripts:        map[string]json.RawMessage{},
		orderedIDs:         []string{"current"},
		respectSearchStart: true,
	}
	imp, st := newTestImporter(t, f)

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	require.EqualValues(1, first.MeetingsAdded)

	f.meetings["backfill"] = meetingFixture(
		t,
		"backfill",
		"2026-06-12T09:00:00Z",
		"2026-05-01T09:00:00Z",
		"2026-05-01T10:00:00Z",
	)
	f.orderedIDs = []string{"current", "backfill"}

	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(1, second.MeetingsAdded,
		"incremental discovery must not hide newly created meetings behind a scheduled-date filter")
	assert.Empty(f.lastSearchStart, "incremental discovery must enumerate all meeting IDs")

	existing, err := st.MessageExistsBatch(first.SourceID, []string{"meeting:backfill"})
	require.NoError(err)
	assert.Contains(existing, "meeting:backfill")
}

func TestImport_RefreshesKnownBackfillWhenSearchSummaryOmitsCreatedAt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	const id = "backfill"
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			id: meetingFixture(t, id, "2026-06-12T09:00:00Z", "2026-05-01T09:00:00Z", "2026-05-01T10:00:00Z"),
		},
		searchMeetings: map[string]json.RawMessage{},
		transcripts:    map[string]json.RawMessage{},
		orderedIDs:     []string{id},
	}
	imp, st := newTestImporter(t, f)

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	require.EqualValues(1, first.MeetingsAdded)

	f.meetings[id] = json.RawMessage(`{
		"id":"backfill",
		"name":"Backfilled meeting refreshed",
		"createdAt":"2026-06-12T09:00:00Z",
		"startTime":"2026-05-01T09:00:00Z",
		"endTime":"2026-05-01T10:00:00Z"
	}`)
	f.searchMeetings[id] = json.RawMessage(`{
		"id":"backfill",
		"startTime":"2026-05-01T09:00:00Z",
		"endTime":"2026-05-01T10:00:00Z"
	}`)

	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(1, second.MeetingsUpdated,
		"missing summary creation time must fail open to hydration")

	var subject string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT subject FROM messages WHERE source_id = ? AND source_message_id = ?`),
		first.SourceID, "meeting:"+id,
	).Scan(&subject))
	assert.Equal("Backfilled meeting refreshed", subject)
}

func TestImport_SkipsKnownOldMeetingWhenSearchSummaryOmitsCreatedAt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"current": meetingFixture(t, "current", "2026-06-12T09:00:00Z", "2026-06-12T09:00:00Z", "2026-06-12T10:00:00Z"),
			"old":     meetingFixture(t, "old", "2026-05-01T09:00:00Z", "2026-05-01T09:00:00Z", "2026-05-01T10:00:00Z"),
		},
		searchMeetings: map[string]json.RawMessage{},
		transcripts:    map[string]json.RawMessage{},
		orderedIDs:     []string{"current", "old"},
	}
	imp, _ := newTestImporter(t, f)

	first, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	require.EqualValues(2, first.MeetingsAdded)

	f.searchMeetings["old"] = json.RawMessage(`{
		"id":"old",
		"startTime":"2026-05-01T09:00:00Z",
		"endTime":"2026-05-01T10:00:00Z"
	}`)
	f.readIDs = nil
	f.transcriptIDs = nil

	second, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(0, second.MeetingsUpdated)
	assert.NotContains(f.readIDs, "old",
		"archived creation time should avoid hydrating an old known meeting")
	assert.NotContains(f.transcriptIDs, "old",
		"an old known meeting must be filtered before transcript retrieval")
}

func TestImport_SearchDateBounds(t *testing.T) {
	t.Run("incremental discovery is unbounded", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := &fakeSource{
			meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
			transcripts: map[string]json.RawMessage{},
		}
		imp, _ := newTestImporter(t, f)

		_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
		require.NoError(err)
		_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
		require.NoError(err)

		assert.Empty(f.lastSearchStart,
			"a creation watermark must not be sent as a scheduled-date lower bound")
	})

	t.Run("created after", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		f := &fakeSource{
			meetings:    map[string]json.RawMessage{},
			transcripts: map[string]json.RawMessage{},
		}
		imp, _ := newTestImporter(t, f)
		plusTwo := time.FixedZone("UTC+2", 2*60*60)
		createdAfter := time.Date(2026, 6, 10, 0, 30, 0, 0, plusTwo)

		_, err := imp.Import(context.Background(), ImportOptions{
			Identifier:   "alice@example.com",
			Full:         true,
			CreatedAfter: createdAfter,
		})
		require.NoError(err)

		assert.Equal("2026-06-09", f.lastSearchStart,
			"the lower bound must convert to UTC before taking its date")
	})
}

func TestImport_LimitDoesNotAdvanceCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	newerID := "42"
	olderID := "7"
	olderMeeting := json.RawMessage(`{
		"id": 7,
		"name": "Older Planning Meeting",
		"createdAt": "2026-06-01T09:00:00Z",
		"startTime": "2026-06-01T09:00:00Z"
	}`)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			newerID: json.RawMessage(meeting42),
			olderID: olderMeeting,
		},
		transcripts:        map[string]json.RawMessage{},
		orderedIDs:         []string{newerID, olderID},
		respectSearchStart: true,
	}
	imp, st := newTestImporter(t, f)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com", Limit: 1})
	require.NoError(err)
	require.EqualValues(1, sum.MeetingsProcessed)

	var cursorJSON string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT cursor_after FROM sync_runs
		WHERE source_id = ? AND status = 'completed'
		ORDER BY id DESC LIMIT 1`), sum.SourceID).Scan(&cursorJSON))
	var cursor syncState
	require.NoError(json.Unmarshal([]byte(cursorJSON), &cursor))
	assert.Empty(cursor.CreatedAfter, "limited run must preserve the prior cursor")

	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(2, count, "normal incremental run must still import the older meeting")
}

func TestImport_MissingReadResultDoesNotAdvanceCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	newerID := "42"
	olderID := "7"
	olderMeeting := json.RawMessage(`{
		"id": 7,
		"name": "Older Planning Meeting",
		"createdAt": "2026-06-01T09:00:00Z",
		"startTime": "2026-06-01T09:00:00Z"
	}`)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			newerID: json.RawMessage(meeting42),
			olderID: olderMeeting,
		},
		transcripts:        map[string]json.RawMessage{},
		omitRead:           map[string]bool{olderID: true},
		orderedIDs:         []string{newerID, olderID},
		respectSearchStart: true,
	}
	imp, st := newTestImporter(t, f)
	priorState := syncState{
		CreatedAfter: "2026-05-31T00:00:00Z",
		PendingTranscripts: []pendingTranscript{{
			MeetingID:     "pending-keep",
			NextAttemptAt: "2026-07-10T12:00:00Z",
			RetryUntil:    "2026-07-16T12:00:00Z",
		}},
	}
	prior := seedCirclebackState(t, st, priorState)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.Error(err)
	assert.EqualValues(1, sum.Errors, "missing detail result must make the traversal incomplete")
	latest, latestErr := st.GetLatestSync(sum.SourceID)
	require.NoError(latestErr)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.EqualValues(1, latest.MessagesProcessed)
	assert.EqualValues(1, latest.ErrorsCount)
	lastSuccessful, lastErr := st.GetLastSuccessfulSync(sum.SourceID)
	require.NoError(lastErr)
	assert.Equal(prior.ID, lastSuccessful.ID)
	assert.Equal(prior.CursorAfter, lastSuccessful.CursorAfter,
		"missing ReadMeetings result must retain the prior watermark and pending list")

	f.omitRead[olderID] = false
	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(2, count, "retry must still import the previously omitted meeting")
}

func TestImport_TranscriptProviderUnrecognizedAndIngestFailuresRetainCursor(t *testing.T) {
	tests := []struct {
		name          string
		transcript    json.RawMessage
		transcriptErr error
		ingestFailure bool
	}{
		{
			name:          "provider tool failure",
			transcriptErr: context.DeadlineExceeded,
		},
		{
			name:       "unrecognized result object",
			transcript: json.RawMessage(`{"id":"hard-error","segments":[{"utterance":"unsupported"}]}`),
		},
		{
			name:          "unrecognized client contract error",
			transcriptErr: fmt.Errorf("%w: unsupported transcript shape", ErrContract),
		},
		{
			name:          "ingest failure",
			transcript:    json.RawMessage(`{"id":"hard-error","text":"ready"}`),
			ingestFailure: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.ingestFailure {
				testutil.SkipIfPostgres(t, "uses a SQLite trigger to inject a canonical ingest failure")
			}
			assert := assert.New(t)
			require := require.New(t)
			meeting := meetingFixture(t, "hard-error", "2026-07-09T10:00:00Z", "2026-07-09T09:00:00Z", "")
			f := &fakeSource{
				meetings:      map[string]json.RawMessage{"hard-error": meeting},
				transcripts:   map[string]json.RawMessage{},
				transcriptErr: tt.transcriptErr,
				orderedIDs:    []string{"hard-error"},
			}
			if tt.transcript != nil {
				f.transcripts["hard-error"] = tt.transcript
			}
			imp, st := newTestImporter(t, f)
			priorState := syncState{
				CreatedAfter: "2026-07-08T00:00:00Z",
				PendingTranscripts: []pendingTranscript{{
					MeetingID:     "pending-keep",
					NextAttemptAt: "2026-07-10T12:00:00Z",
					RetryUntil:    "2026-07-16T12:00:00Z",
				}},
			}
			prior := seedCirclebackState(t, st, priorState)
			if tt.ingestFailure {
				_, triggerErr := st.DB().Exec(`
					CREATE TRIGGER fail_circleback_task3_ingest
					BEFORE INSERT ON message_raw
					WHEN NEW.raw_format = 'circleback_json'
					BEGIN
					SELECT RAISE(ABORT, 'forced Circleback ingest failure');
					END
				`)
				require.NoError(triggerErr)
			}

			sum, importErr := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})

			require.Error(importErr)
			require.NotNil(sum)
			assert.EqualValues(1, sum.MeetingsProcessed)
			assert.EqualValues(1, sum.Errors)
			latest, latestErr := st.GetLatestSync(prior.SourceID)
			require.NoError(latestErr)
			assert.Equal(store.SyncStatusFailed, latest.Status)
			assert.Equal(sum.MeetingsProcessed, latest.MessagesProcessed)
			assert.Equal(sum.MeetingsAdded, latest.MessagesAdded)
			assert.Equal(sum.Errors, latest.ErrorsCount)
			lastSuccessful, successErr := st.GetLastSuccessfulSync(prior.SourceID)
			require.NoError(successErr)
			assert.Equal(prior.ID, lastSuccessful.ID)
			assert.Equal(prior.CursorAfter, lastSuccessful.CursorAfter,
				"hard failure must retain the exact prior watermark and pending list")
			var messageCount int
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`),
				prior.SourceID, "meeting:hard-error").Scan(&messageCount))
			assert.Zero(messageCount,
				"transcript provider, contract, and ingest failures must leave new meetings retryable")
		})
	}
}

func TestImport_ExhaustsSearchPagesBeforeAdvancingCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings: map[string]json.RawMessage{
			"42": json.RawMessage(meeting42),
			"7": json.RawMessage(`{
				"id": 7,
				"name": "Older Planning Meeting",
				"createdAt": "2026-06-01T09:00:00Z",
				"startTime": "2026-06-01T09:00:00Z"
			}`),
		},
		transcripts: map[string]json.RawMessage{},
		searchPages: [][]string{{"42"}, {"7"}, {}},
	}
	imp, st := newTestImporter(t, f)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(2, sum.MeetingsProcessed)
	assert.Equal([]int{0, 1, 2}, f.searchedPages)

	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
	assert.Equal(2, count)
}

func TestImport_CancellationMarksSyncFailed(t *testing.T) {
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{},
	}
	imp, st := newTestImporter(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sum, err := imp.Import(ctx, ImportOptions{Identifier: "alice@example.com"})
	require.ErrorIs(err, context.Canceled)

	var status string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT status FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`),
		sum.SourceID).Scan(&status))
	require.Equal("failed", status)
}

func TestImport_TranscriptFailureStillArchivesNotes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{}, // no transcript available
	}
	imp, st := newTestImporter(t, f)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsAdded)

	var body string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT mb.body_text FROM message_bodies mb
		JOIN messages m ON m.id = mb.message_id
		WHERE m.source_message_id = ?`), "meeting:42").Scan(&body))
	assert.Contains(body, "Ship the new layout")
	assert.NotContains(body, "Transcript:")
}

func TestSnippetPreservesUTF8(t *testing.T) {
	assert := assert.New(t)
	body := strings.Repeat("a", 199) + "é" + "tail"

	got := snippet(body)

	assert.True(utf8.ValidString(got))
	assert.Equal(strings.Repeat("a", 199)+"é", got)
}

func TestImport_TranscriptFailurePreservesArchivedTranscript(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
	}
	imp, st := newTestImporter(t, f)

	// Clean run archives the transcript.
	_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)

	bodyOf := func(smid string) string {
		var body string
		require.NoError(st.DB().QueryRow(st.Rebind(`
			SELECT mb.body_text FROM message_bodies mb
			JOIN messages m ON m.id = mb.message_id
			WHERE m.source_message_id = ?`), smid).Scan(&body))
		return body
	}
	require.Contains(bodyOf("meeting:42"), "[00:00] Alice Smith: Welcome to the design review.")

	// Transcript retrieval now fails while the meeting notes were edited
	// server-side, and a brand-new historical meeting appears. The existing
	// meeting must keep its archived transcript; the new one must remain absent
	// so the next incremental search still treats it as retryable even though its
	// creation time is outside the refresh overlap.
	f.failTranscripts = true
	f.meetings["42"] = json.RawMessage(`{
		"id": 42, "name": "Design Review",
		"createdAt": "2026-06-10T17:05:00Z", "startTime": "2026-06-10T17:00:00Z",
		"organizer": {"name": "Alice Smith", "email": "alice@example.com"},
		"attendees": [{"name": "Alice Smith", "email": "alice@example.com"}],
		"notes": "EDITED notes that must not clobber the transcript"
	}`)
	f.meetings["43"] = json.RawMessage(`{
		"id": 43, "name": "Retro",
		"createdAt": "2026-06-11T10:00:00Z", "startTime": "2026-06-11T10:00:00Z",
		"organizer": {"name": "Alice Smith", "email": "alice@example.com"},
		"attendees": [{"name": "Alice Smith", "email": "alice@example.com"}],
		"notes": "Retro notes"
	}`)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.Error(err)
	assert.Positive(sum.Errors, "transcript failure must count as an error (holds the watermark)")
	assert.Zero(sum.MeetingsAdded, "a new meeting without a fetched transcript remains retryable")

	body42 := bodyOf("meeting:42")
	assert.Contains(body42, "[00:00] Alice Smith: Welcome to the design review.",
		"existing archived transcript must survive a transient transcript failure")
	assert.NotContains(body42, "EDITED notes", "the skipped meeting is untouched until transcripts return")

	var meeting43Count int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages WHERE source_message_id = ?`), "meeting:43").Scan(&meeting43Count))
	assert.Zero(meeting43Count, "historical meeting must not be persisted without durable retry state")

	// Once transcripts are available again, the edited meeting refreshes
	// fully (notes + transcript) because the held watermark re-covers it.
	f.failTranscripts = false
	f.transcripts["43"] = json.RawMessage(`{"meetingId": 43, "transcript": [
		{"speaker": "Alice Smith", "text": "What went well?", "start": 0}
	]}`)
	_, err = imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	body42 = bodyOf("meeting:42")
	assert.Contains(body42, "EDITED notes")
	assert.Contains(body42, "[00:00] Alice Smith: Welcome to the design review.")
	assert.Contains(bodyOf("meeting:43"), "[00:00] Alice Smith: What went well?")
}

func TestImport_OmittedTranscriptPreservesArchive(t *testing.T) {
	for _, archive := range archivedTranscriptFixtures {
		for _, refresh := range transcriptRefreshFixtures {
			t.Run(archive.name+"/"+refresh.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := &fakeSource{
					meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
					transcripts: map[string]json.RawMessage{"42": archive.payload},
				}
				imp, st := newTestImporter(t, f)

				initial, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
				require.NoError(err)
				originalID := circlebackMessageID(t, st)
				require.Contains(circlebackMessageBody(t, st, originalID), archive.bodyText)
				labelID, err := st.EnsureLabel(initial.SourceID, "circleback-test-label", "Keep", "user")
				require.NoError(err)
				require.NoError(st.AddMessageLabels(originalID, []int64{labelID}))

				f.meetings["42"] = json.RawMessage(refreshedMeeting42)
				if refresh.payload == nil {
					delete(f.transcripts, "42")
				} else {
					f.transcripts["42"] = refresh.payload
				}

				sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
				require.NoError(err)
				assert.EqualValues(0, sum.Errors)
				assert.EqualValues(0, sum.MeetingsAdded)

				refreshedID := circlebackMessageID(t, st)
				assert.Equal(originalID, refreshedID, "refresh must rewrite the canonical message row")
				var count int
				require.NoError(st.DB().QueryRow(st.Rebind(
					`SELECT COUNT(*) FROM messages WHERE source_id = ? AND source_message_id = ?`),
					sum.SourceID, "meeting:42").Scan(&count))
				assert.Equal(1, count)
				require.NoError(st.DB().QueryRow(st.Rebind(
					`SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ?`),
					refreshedID, labelID).Scan(&count))
				assert.Equal(1, count, "canonical refresh must preserve user-added labels")

				var subject string
				require.NoError(st.DB().QueryRow(st.Rebind(
					`SELECT subject FROM messages WHERE id = ?`), refreshedID).Scan(&subject))
				assert.Equal("Design Review Refreshed", subject)
				body := circlebackMessageBody(t, st, refreshedID)
				assert.Contains(body, "Refreshedsignal notes from the current meeting payload")
				assert.Contains(body, archive.bodyText)
				assert.NotContains(body, "Ship the new layout")

				metadata := circlebackMetadataMap(t, st, refreshedID)
				assert.Equal("2026-06-12T09:00:00Z", metadata["created_at"])
				assert.Equal("present", metadata["transcript_state"])
				assert.InDelta(float64(3600), metadata["duration_seconds"], 0)

				raw, rawErr := st.GetMessageRaw(refreshedID)
				require.NoError(rawErr)
				var composed map[string]json.RawMessage
				require.NoError(json.Unmarshal(raw, &composed))
				assert.JSONEq(refreshedMeeting42, string(composed["meeting"]))
				assert.JSONEq(string(archive.payload), string(composed["transcript"]))
			})
		}
	}
}

func TestImport_ArchivedTranscriptRecovery(t *testing.T) {
	t.Run("explicit present raw archive is authoritative", func(t *testing.T) {
		for _, archive := range archivedTranscriptFixtures {
			t.Run(archive.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := &fakeSource{
					meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
					transcripts: map[string]json.RawMessage{"42": archive.payload},
				}
				imp, st := newTestImporter(t, f)
				_, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
				require.NoError(err)
				msgID := circlebackMessageID(t, st)

				f.meetings["42"] = json.RawMessage(refreshedMeeting42)
				delete(f.transcripts, "42")
				sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
				require.NoError(err)
				assert.EqualValues(0, sum.Errors)
				body := circlebackMessageBody(t, st, msgID)
				assert.Contains(body, "Refreshedsignal notes")
				assert.Contains(body, archive.bodyText)
				assert.Equal("present", circlebackMetadataMap(t, st, msgID)["transcript_state"])
			})
		}
	})

	t.Run("known present but unrecoverable archive fails without mutation", func(t *testing.T) {
		tests := []struct {
			name        string
			setMetadata func(t *testing.T, st *store.Store, msgID int64)
			corruptRaw  func(t *testing.T, st *store.Store, msgID int64)
		}{
			{
				name: "explicit present state with missing transcript raw",
				setMetadata: func(t *testing.T, st *store.Store, msgID int64) {
					t.Helper()
					metadata := circlebackMetadataMap(t, st, msgID)
					metadata["transcript_state"] = "present"
					encoded, err := json.Marshal(metadata)
					require.NoError(t, err)
					require.NoError(t, st.SetMessageMetadata(msgID,
						sql.NullString{String: string(encoded), Valid: true}))
				},
				corruptRaw: func(t *testing.T, st *store.Store, msgID int64) {
					t.Helper()
					require.NoError(t, st.UpsertMessageRawWithFormat(msgID,
						[]byte(`{"meeting":{"id":42}}`), RawFormat))
				},
			},
			{
				name: "missing explicit transcript state",
				setMetadata: func(t *testing.T, st *store.Store, msgID int64) {
					t.Helper()
					metadata := circlebackMetadataMap(t, st, msgID)
					delete(metadata, "transcript_state")
					encoded, err := json.Marshal(metadata)
					require.NoError(t, err)
					require.NoError(t, st.SetMessageMetadata(msgID,
						sql.NullString{String: string(encoded), Valid: true}))
				},
				corruptRaw: func(t *testing.T, st *store.Store, msgID int64) {
					t.Helper()
				},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := &fakeSource{
					meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
					transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
				}
				imp, st := newTestImporter(t, f)
				initial, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
				require.NoError(err)
				msgID := circlebackMessageID(t, st)
				priorSuccessful, err := st.GetLastSuccessfulSync(initial.SourceID)
				require.NoError(err)

				tt.setMetadata(t, st, msgID)
				tt.corruptRaw(t, st, msgID)
				bodyBefore := circlebackMessageBody(t, st, msgID)
				rawBefore, err := st.GetMessageRaw(msgID)
				require.NoError(err)
				metadataBefore, err := st.GetMessageMetadata(msgID)
				require.NoError(err)

				f.meetings["42"] = json.RawMessage(refreshedMeeting42)
				delete(f.transcripts, "42")
				sum, importErr := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
				require.Error(importErr)
				require.NotNil(sum)
				assert.EqualValues(1, sum.MeetingsProcessed)
				assert.EqualValues(0, sum.MeetingsAdded)
				assert.EqualValues(1, sum.Errors)

				latest, err := st.GetLatestSync(initial.SourceID)
				require.NoError(err)
				assert.Equal(store.SyncStatusFailed, latest.Status)
				assert.EqualValues(1, latest.MessagesProcessed)
				assert.EqualValues(0, latest.MessagesAdded)
				assert.EqualValues(1, latest.ErrorsCount)
				assert.True(latest.ErrorMessage.Valid)

				lastSuccessful, err := st.GetLastSuccessfulSync(initial.SourceID)
				require.NoError(err)
				assert.Equal(priorSuccessful.ID, lastSuccessful.ID,
					"failed recovery must retain the prior successful cursor row")
				assert.Equal(priorSuccessful.CursorAfter, lastSuccessful.CursorAfter)

				assert.Equal(bodyBefore, circlebackMessageBody(t, st, msgID))
				rawAfter, err := st.GetMessageRaw(msgID)
				require.NoError(err)
				assert.Equal(rawBefore, rawAfter)
				metadataAfter, err := st.GetMessageMetadata(msgID)
				require.NoError(err)
				assert.Equal(metadataBefore, metadataAfter)
			})
		}
	})
}

func TestIngestMeeting_RawFailureRollsBackCanonicalRefresh(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to inject a raw-archive write failure")
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
	}
	imp, st := newTestImporter(t, f)
	require.True(st.FTS5Available(), "atomic rollback test inspects the SQLite FTS row")
	initial, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	msgID := circlebackMessageID(t, st)
	before := circlebackPersistenceSnapshot(t, st, msgID)

	_, err = st.DB().Exec(`
		CREATE TRIGGER fail_circleback_raw_archive
		BEFORE INSERT ON message_raw
		WHEN NEW.raw_format = 'circleback_json'
		BEGIN
			SELECT RAISE(ABORT, 'forced circleback raw failure');
		END
	`)
	require.NoError(err)

	var refreshed Meeting
	require.NoError(json.Unmarshal([]byte(refreshedMeeting42), &refreshed))
	refreshed.Raw = json.RawMessage(refreshedMeeting42)
	recoveredTranscript, err := decodeTranscript(json.RawMessage(transcript42))
	require.NoError(err)

	added, changed, err := imp.ingestMeeting(
		initial.SourceID, "alice@example.com", nil, &refreshed, recoveredTranscript, transcriptStatePresent, false,
	)
	require.Error(err)
	assert.False(added)
	assert.False(changed)
	assert.Contains(err.Error(), "upsert raw")
	assert.Equal(before, circlebackPersistenceSnapshot(t, st, msgID),
		"a late canonical write failure must roll back the message and every related row")
}

func TestImport_MessageLookupFailureMarksSyncFailed(t *testing.T) {
	testutil.SkipIfPostgres(t, "renames the SQLite messages table to force MessageExistsBatch failure")
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
	}
	imp, st := newTestImporter(t, f)
	src, err := st.GetOrCreateSource(SourceType, "alice@example.com")
	require.NoError(err)
	_, err = st.DB().Exec(`ALTER TABLE messages RENAME TO unavailable_messages`)
	require.NoError(err)

	sum, importErr := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.Error(importErr)
	require.NotNil(sum)
	assert.Contains(importErr.Error(), "lookup search page")
	latest, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.True(latest.ErrorMessage.Valid)
}

func TestImport_CheckpointFailureJoinsHardError(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to inject a checkpoint failure")
	assert := assert.New(t)
	require := require.New(t)
	f := &fakeSource{
		meetings:    map[string]json.RawMessage{"42": json.RawMessage(meeting42)},
		transcripts: map[string]json.RawMessage{"42": json.RawMessage(transcript42)},
	}
	imp, st := newTestImporter(t, f)
	initial, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	msgID := circlebackMessageID(t, st)

	metadata := circlebackMetadataMap(t, st, msgID)
	metadata["transcript_state"] = "present"
	encoded, err := json.Marshal(metadata)
	require.NoError(err)
	require.NoError(st.SetMessageMetadata(msgID, sql.NullString{String: string(encoded), Valid: true}))
	require.NoError(st.UpsertMessageRawWithFormat(msgID, []byte(`{"meeting":{"id":42}}`), RawFormat))
	_, err = st.DB().Exec(`
		CREATE TRIGGER fail_circleback_checkpoint
		BEFORE UPDATE OF messages_processed ON sync_runs
		WHEN OLD.status = 'running' AND NEW.status = 'running'
		 AND NEW.messages_processed > OLD.messages_processed
		BEGIN
			SELECT RAISE(ABORT, 'forced circleback checkpoint failure');
		END
	`)
	require.NoError(err)

	f.meetings["42"] = json.RawMessage(refreshedMeeting42)
	delete(f.transcripts, "42")
	sum, importErr := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.Error(importErr)
	require.NotNil(sum)
	assert.Contains(importErr.Error(), "recover archived transcript")
	assert.Contains(importErr.Error(), "checkpoint")
	latest, err := st.GetLatestSync(initial.SourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, latest.Status)
	assert.EqualValues(1, latest.MessagesProcessed)
	assert.EqualValues(2, latest.ErrorsCount,
		"archive recovery and checkpoint failures are separate run-level errors")
}

func TestImport_NotesOnlyMeetingDoesNotStallWatermark(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// A meeting the server never returns a transcript for (notes-only) must
	// import cleanly on every run — flagging it as an error would hold the
	// watermark forever.
	f := &fakeSource{
		meetings: map[string]json.RawMessage{"44": json.RawMessage(`{
			"id": 44, "name": "Async Update",
			"createdAt": "2026-06-15T09:00:00Z", "startTime": "2026-06-15T09:00:00Z",
			"organizer": {"name": "Alice Smith", "email": "alice@example.com"},
			"attendees": [{"name": "Alice Smith", "email": "alice@example.com"}],
			"notes": "Written update, no recording"
		}`)},
		transcripts: map[string]json.RawMessage{},
	}
	imp, st := newTestImporter(t, f)

	sum, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(1, sum.MeetingsAdded)
	assert.EqualValues(0, sum.Errors, "a new notes-only meeting is not an error")

	// Re-run: still omitted, still clean, notes refresh in place.
	sum2, err := imp.Import(context.Background(), ImportOptions{Identifier: "alice@example.com"})
	require.NoError(err)
	assert.EqualValues(0, sum2.Errors, "an archived notes-only meeting must not error on re-sync")

	var blob string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT cursor_after FROM sync_runs
		WHERE source_id = ? AND status = 'completed'
		ORDER BY id DESC LIMIT 1`), sum.SourceID).Scan(&blob))
	var state syncState
	require.NoError(json.Unmarshal([]byte(blob), &state))
	assert.Equal("2026-06-15T09:00:00Z", state.CreatedAfter, "watermark advances despite the omitted transcript")
}

func circlebackMessageID(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var msgID int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT id FROM messages WHERE source_message_id = ?`), "meeting:42").Scan(&msgID))
	return msgID
}

func circlebackMessageBody(t *testing.T, st *store.Store, msgID int64) string {
	t.Helper()
	var body string
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT body_text FROM message_bodies WHERE message_id = ?`), msgID).Scan(&body))
	return body
}

func circlebackMetadataMap(t *testing.T, st *store.Store, msgID int64) map[string]any {
	t.Helper()
	metadataJSON, err := st.GetMessageMetadata(msgID)
	require.NoError(t, err)
	require.True(t, metadataJSON.Valid)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal([]byte(metadataJSON.String), &metadata))
	return metadata
}

type meetingPersistenceSnapshot struct {
	Subject                  sql.NullString
	Snippet                  sql.NullString
	SizeEstimate             int64
	HasAttachments           bool
	AttachmentCount          int
	Metadata                 sql.NullString
	Body                     sql.NullString
	Raw                      string
	ConversationTitle        sql.NullString
	Recipients               []string
	ConversationParticipants []string
	Attachments              []string
	FTS                      []string
}

func circlebackPersistenceSnapshot(t *testing.T, st *store.Store, msgID int64) meetingPersistenceSnapshot {
	t.Helper()
	var snapshot meetingPersistenceSnapshot
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT m.subject, m.snippet, m.size_estimate, m.has_attachments,
		       m.attachment_count, m.metadata, c.title
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		WHERE m.id = ?`), msgID).Scan(
		&snapshot.Subject, &snapshot.Snippet, &snapshot.SizeEstimate,
		&snapshot.HasAttachments, &snapshot.AttachmentCount,
		&snapshot.Metadata, &snapshot.ConversationTitle,
	))
	require.NoError(t, st.DB().QueryRow(st.Rebind(
		`SELECT body_text FROM message_bodies WHERE message_id = ?`), msgID).Scan(&snapshot.Body))
	raw, err := st.GetMessageRaw(msgID)
	require.NoError(t, err)
	snapshot.Raw = string(raw)
	snapshot.Recipients = circlebackStringRows(t, st, `
		SELECT recipient_type || ':' || participant_id || ':' || COALESCE(display_name, '')
		FROM message_recipients
		WHERE message_id = ?
		ORDER BY recipient_type, participant_id`, msgID)
	snapshot.ConversationParticipants = circlebackStringRows(t, st, `
		SELECT cp.participant_id || ':' || COALESCE(cp.role, '')
		FROM conversation_participants cp
		JOIN messages m ON m.conversation_id = cp.conversation_id
		WHERE m.id = ?
		ORDER BY cp.participant_id`, msgID)
	snapshot.Attachments = circlebackStringRows(t, st, `
		SELECT COALESCE(filename, '') || ':' || COALESCE(storage_path, '')
		FROM attachments
		WHERE message_id = ?
		ORDER BY id`, msgID)
	var ftsSubject, ftsBody, ftsFrom, ftsTo, ftsCC string
	ftsErr := st.DB().QueryRow(st.Rebind(`
		SELECT subject, body, from_addr, to_addr, cc_addr
		FROM messages_fts WHERE rowid = ?`), msgID).Scan(
		&ftsSubject, &ftsBody, &ftsFrom, &ftsTo, &ftsCC,
	)
	if ftsErr == nil {
		snapshot.FTS = []string{ftsSubject, ftsBody, ftsFrom, ftsTo, ftsCC}
	} else {
		require.ErrorIs(t, ftsErr, sql.ErrNoRows)
	}
	return snapshot
}

func circlebackStringRows(t *testing.T, st *store.Store, query string, args ...any) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(query), args...)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var values []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		values = append(values, value)
	}
	require.NoError(t, rows.Err())
	return values
}
