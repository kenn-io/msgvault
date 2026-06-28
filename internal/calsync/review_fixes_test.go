package calsync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// TestFull_LimitDoesNotAdvanceCursor is the regression for the silent-data-loss
// bug where a --limit full sync on a single-page calendar captured the final
// nextSyncToken and advanced the cursor past the un-ingested events, so the next
// incremental sync would never see them.
func TestFull_LimitDoesNotAdvanceCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	// One page of 5 events (the common single-page case) with a terminal token.
	var evs []gcal.Event
	for i := range 5 {
		evs = append(evs, timedEvent("ev"+string(rune('a'+i)), "Event"))
	}
	m.FullEvents["primary"] = [][]gcal.Event{evs}
	m.FullSyncToken["primary"] = "TOKEN1"

	s, st := newSyncer(t, m, Options{Limit: 2})
	_, err := s.Full(context.Background())
	require.NoError(err)

	src := primarySource(t, st)
	assert.Equal(2, countMessages(t, st, src.ID), "only --limit events ingested")
	assert.Empty(src.SyncCursor.String, "a --limit run must NOT advance the incremental cursor")

	// A later unlimited full sync re-traverses everything and DOES advance.
	s2 := New(m, st, Options{AccountEmail: testAccount}).WithLogger(quietLogger())
	_, err = s2.Full(context.Background())
	require.NoError(err)
	src = primarySource(t, st)
	assert.Equal(5, countMessages(t, st, src.ID), "unlimited run ingests all events")
	assert.Equal("TOKEN1", src.SyncCursor.String, "complete run advances the cursor")
}

// TestReingest_ClearsStaleRecipients is the regression for stale 'from'/'to'
// rows surviving when an event loses its organizer or all attendees on re-sync.
func TestReingest_ClearsStaleRecipients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "Meeting",
		gcal.Attendee{Email: "bob@example.com", DisplayName: "Bob"},
		gcal.Attendee{Email: "carol@example.com", DisplayName: "Carol"})}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})
	_, err := s.Full(context.Background())
	require.NoError(err)
	src := primarySource(t, st)
	row, ok := getMsg(t, st, src.ID, "e1")
	require.True(ok)
	require.Equal([]string{"bob@example.com", "carol@example.com"}, recipientEmails(t, st, row.id, "to"))
	require.Equal([]string{testAccount}, recipientEmails(t, st, row.id, "from"))

	// Re-sync the same event id with NO organizer and NO attendees.
	m.FullEvents["primary"] = [][]gcal.Event{{{
		ID:      "e1",
		Status:  gcal.StatusConfirmed,
		Summary: "Meeting (now solo)",
		Start:   gcal.EventDateTime{DateTime: time.Date(2024, 5, 1, 16, 0, 0, 0, time.UTC)},
		End:     gcal.EventDateTime{DateTime: time.Date(2024, 5, 1, 16, 30, 0, 0, time.UTC)},
	}}}
	_, err = s.Full(context.Background())
	require.NoError(err)

	row2, ok := getMsg(t, st, src.ID, "e1")
	require.True(ok)
	assert.Equal(row.id, row2.id, "same message row updated")
	assert.Empty(recipientEmails(t, st, row2.id, "to"), "stale attendee rows must be cleared")
	assert.Empty(recipientEmails(t, st, row2.id, "from"), "stale organizer row must be cleared")
}

// TestFull_DuplicateAttendeesDoNotCollide is the regression for the production
// UNIQUE-constraint crash: a calendar event that lists the same person twice (a
// duplicate attendee entry, common on busy recurring work/personal calendars)
// made the 'to' recipient insert collide on
// (message_id, participant_id, recipient_type) and aborted the ENTIRE calendar's
// sync. The duplicate must collapse to one 'to' row and the sync must succeed.
func TestFull_DuplicateAttendeesDoNotCollide(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("dup1", "Standup",
		gcal.Attendee{Email: "bob@example.com", DisplayName: "Bob"},
		gcal.Attendee{Email: "bob@example.com", DisplayName: "Bob (again)"},
		gcal.Attendee{Email: "carol@example.com", DisplayName: "Carol"})}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})
	_, err := s.Full(context.Background())
	require.NoError(err, "an event with duplicate attendees must not abort the sync")

	src := primarySource(t, st)
	row, ok := getMsg(t, st, src.ID, "dup1")
	require.True(ok)
	assert.Equal([]string{"bob@example.com", "carol@example.com"}, recipientEmails(t, st, row.id, "to"),
		"a duplicate attendee collapses to a single 'to' row")
}

// TestFull_BoundedSyncDoesNotAdvanceCursor is the regression (from the
// adversarial audit) for silent data loss when a time-bounded full sync
// (--after/--before) established an incremental baseline scoped to only that
// window: future incremental syncs carry no time bounds, so out-of-window events
// would never be archived. A bounded run must ingest its window but NOT advance
// the cursor — exactly like a --limit run.
func TestFull_BoundedSyncDoesNotAdvanceCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{
		timedEvent("a", "A"), timedEvent("b", "B"),
	}}
	m.FullSyncToken["primary"] = "TOKEN1"

	s, st := newSyncer(t, m, Options{TimeMin: "2024-01-01T00:00:00Z"})
	_, err := s.Full(context.Background())
	require.NoError(err)

	src := primarySource(t, st)
	assert.Equal(2, countMessages(t, st, src.ID), "bounded full sync still ingests in-window events")
	assert.Empty(src.SyncCursor.String, "a time-bounded run must NOT advance the incremental cursor")
}

func TestIncremental_AppliesAccessRoleSelection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{
		{ID: "primary", AccessRole: "owner"},
		{ID: "holidays", AccessRole: "reader"},
	}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("p1", "Primary")}}
	m.FullEvents["holidays"] = [][]gcal.Event{{timedEvent("h1", "Holiday")}}
	m.FullSyncToken["primary"] = "P1"
	m.FullSyncToken["holidays"] = "H1"

	s, st := newSyncer(t, m, Options{AllCalendars: true})
	_, err := s.Full(context.Background())
	require.NoError(err)

	primary, err := st.GetSourceByIdentifier(testAccount + "/primary")
	require.NoError(err)
	holidays, err := st.GetSourceByIdentifier(testAccount + "/holidays")
	require.NoError(err)
	require.Equal("P1", primary.SyncCursor.String)
	require.Equal("H1", holidays.SyncCursor.String)

	m.IncEvents["P1"] = [][]gcal.Event{{timedEvent("p2", "Primary delta")}}
	m.IncNextToken["P1"] = "P2"
	m.IncEvents["H1"] = [][]gcal.Event{{timedEvent("h2", "Holiday delta")}}
	m.IncNextToken["H1"] = "H2"

	defaultSelection := New(m, st, Options{AccountEmail: testAccount}).WithLogger(quietLogger())
	res, err := defaultSelection.Incremental(context.Background())

	require.NoError(err)
	assert.Equal(1, res.CalendarsSynced, "default incremental sync should keep owner+writer only")
	assert.Equal(2, countMessages(t, st, primary.ID), "owner calendar should receive its delta")
	assert.Equal(1, countMessages(t, st, holidays.ID), "reader calendar should be skipped by default")

	holidays, err = st.GetSourceByIdentifier(testAccount + "/holidays")
	require.NoError(err)
	assert.Equal("H1", holidays.SyncCursor.String, "skipped reader calendar cursor must not advance")
}

func TestIncremental_MissingAccessRoleOnRegisteredSourceStillSyncs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.IncEvents["T1"] = [][]gcal.Event{{timedEvent("legacy-delta", "Legacy delta")}}
	m.IncNextToken["T1"] = "T2"

	s, st := newSyncer(t, m, Options{AccountEmail: testAccount})
	src, err := st.GetOrCreateSource(gcal.SourceType, testAccount+"/legacy")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(src.ID,
		`{"account_email":"alice@example.com","calendar_id":"legacy"}`))
	require.NoError(st.UpdateSourceSyncCursor(src.ID, "T1"))

	res, err := s.Incremental(context.Background())

	require.NoError(err)
	assert.Equal(1, res.CalendarsSynced, "legacy registered source should not be filtered by missing access_role")
	assert.Equal(1, res.EventsAdded)
	src, err = st.GetSourceByIdentifier(testAccount + "/legacy")
	require.NoError(err)
	assert.Equal("T2", src.SyncCursor.String)
	assert.Equal(1, countMessages(t, st, src.ID))
}

func TestFull_FTSFailureDoesNotAbortCalendarSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "FTS failure")}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})
	if !st.FTS5Available() || st.IsPostgreSQL() {
		t.Skip("SQLite FTS5-specific regression")
	}
	_, err := st.DB().Exec("DROP TABLE messages_fts")
	require.NoError(err, "break FTS table")

	res, err := s.Full(context.Background())

	require.NoError(err, "FTS indexing failure must not abort event persistence")
	assert.Equal(1, res.EventsAdded)
	src := primarySource(t, st)
	_, ok := getMsg(t, st, src.ID, "e1")
	assert.True(ok, "event row should still be persisted")
	assert.Equal("T1", src.SyncCursor.String, "cursor should advance after durable event persistence")
}

func TestFull_BoundedInterruptedRunDoesNotWriteResumePageToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{
		{timedEvent("e1", "One")},
		{timedEvent("e2", "Two")},
	}
	m.FullSyncToken["primary"] = "T1"

	st := testutil.NewTestStore(t)
	failing := &listEventsRecorder{
		MockAPI:    m,
		failOnCall: 2,
		err:        errors.New("boom"),
	}
	bounded := New(failing, st, Options{
		AccountEmail: testAccount,
		TimeMin:      "2025-01-01T00:00:00Z",
	}).WithLogger(quietLogger())

	_, err := bounded.Full(context.Background())
	require.Error(err)

	src := primarySource(t, st)
	run, err := st.GetLatestSync(src.ID)
	require.NoError(err)
	assert.False(run.CursorBefore.Valid && run.CursorBefore.String != "",
		"bounded failed run must not leave a resumable page token")

	recorder := &listEventsRecorder{MockAPI: m}
	unbounded := New(recorder, st, Options{AccountEmail: testAccount}).WithLogger(quietLogger())
	_, err = unbounded.Full(context.Background())
	require.NoError(err)
	require.NotEmpty(recorder.params)
	assert.Empty(recorder.params[0].PageToken, "later unbounded sync must restart from the first page")
}

func TestFull_LegacyResumeCheckpointIgnored(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{
		{timedEvent("e1", "One")},
		{timedEvent("e2", "Two")},
	}
	m.FullSyncToken["primary"] = "T1"

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource(gcal.SourceType, testAccount+"/primary")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(src.ID, `{"account_email":"`+testAccount+`","calendar_id":"primary"}`))
	oldSyncID, err := st.StartSync(src.ID, "full")
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(oldSyncID, &store.Checkpoint{
		PageToken: "1", MessagesProcessed: 2, MessagesAdded: 2,
	}))

	recorder := &listEventsRecorder{MockAPI: m}
	s := New(recorder, st, Options{AccountEmail: testAccount}).WithLogger(quietLogger())
	_, err = s.Full(context.Background())
	require.NoError(err)

	require.NotEmpty(recorder.params)
	assert.Empty(recorder.params[0].PageToken, "legacy raw page-token checkpoints are not resumable")
}

// TestFull_RecurringConversationTitleFromMasterNotException is the regression
// (from the adversarial audit) for the series conversation title flapping: an
// edited per-instance exception's summary used to overwrite the shared series
// title (last-writer-wins). The exception keeps its own subject, but the series
// conversation title must come from the master.
func TestFull_RecurringConversationTitleFromMasterNotException(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}

	master := gcal.Event{
		ID:         "r1",
		Status:     gcal.StatusConfirmed,
		Summary:    "Weekly sync",
		Organizer:  gcal.Person{Email: testAccount, Self: true},
		Start:      gcal.EventDateTime{DateTime: time.Date(2024, 5, 2, 10, 0, 0, 0, time.UTC)},
		End:        gcal.EventDateTime{DateTime: time.Date(2024, 5, 2, 10, 30, 0, 0, time.UTC)},
		Recurrence: []string{"RRULE:FREQ=WEEKLY;BYDAY=TH"},
	}
	// Confirmed exception with an EDITED summary, delivered AFTER the master —
	// the order that previously overwrote the series title.
	exception := gcal.Event{
		ID:                "r1_20240509T100000Z",
		Status:            gcal.StatusConfirmed,
		Summary:           "Weekly sync — MOVED to Zoom",
		Organizer:         gcal.Person{Email: testAccount, Self: true},
		RecurringEventID:  "r1",
		OriginalStartTime: gcal.EventDateTime{DateTime: time.Date(2024, 5, 9, 10, 0, 0, 0, time.UTC)},
		Start:             gcal.EventDateTime{DateTime: time.Date(2024, 5, 9, 15, 0, 0, 0, time.UTC)},
		End:               gcal.EventDateTime{DateTime: time.Date(2024, 5, 9, 15, 30, 0, 0, time.UTC)},
	}
	m.FullEvents["primary"] = [][]gcal.Event{{master, exception}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})
	_, err := s.Full(context.Background())
	require.NoError(err)
	src := primarySource(t, st)

	masterRow, ok := getMsg(t, st, src.ID, "r1")
	require.True(ok)
	excRow, ok := getMsg(t, st, src.ID, "r1|2024-05-09T10:00:00Z")
	require.True(ok)
	require.Equal(masterRow.convID, excRow.convID, "master and exception share one series conversation")

	assert.Equal("Weekly sync — MOVED to Zoom", excRow.subject.String,
		"the edited summary stays on the exception's own message row")
	assert.Equal("Weekly sync", conversationTitle(t, st, masterRow.convID),
		"the series conversation title comes from the master, not an edited instance")
}

// TestIncremental_PersistErrorDoesNotAdvanceCursor is the regression for the
// incremental path silently dropping an event that fails to persist: the cursor
// must stay put so the next sync re-delivers and retries it.
func TestIncremental_PersistErrorDoesNotAdvanceCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "First")}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})
	_, err := s.Full(context.Background())
	require.NoError(err)
	src := primarySource(t, st)
	require.Equal("T1", src.SyncCursor.String)

	// Force a persist failure during the incremental: drop a table the ingest
	// path writes to, so the new event's UpsertMessageRawWithFormat errors.
	_, err = st.DB().Exec("DROP TABLE message_raw")
	require.NoError(err)

	m.IncEvents["T1"] = [][]gcal.Event{{timedEvent("e2", "Second")}}
	m.IncNextToken["T1"] = "T2"

	_, err = s.Incremental(context.Background())
	require.Error(err, "a per-item persist failure must surface, not be swallowed")

	src2 := primarySource(t, st)
	assert.Equal("T1", src2.SyncCursor.String, "cursor must NOT advance past an event that failed to persist")
}

// TestFull_ResumeSupersedesAndSeedsCounters is the regression for the resume
// path: it must go through StartSync (so a stale/concurrent running run is
// superseded under the writer lock, not shared) and carry the prior run's
// counters forward so a resumed run's stats are not reset to zero.
func TestFull_ResumeSupersedesAndSeedsCounters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{{timedEvent("e1", "One")}}
	m.FullSyncToken["primary"] = "T1"

	s, st := newSyncer(t, m, Options{})

	// Simulate an interrupted prior run: a 'running' sync_run with 2 already
	// processed, checkpointed to resume from the first page ("").
	src, err := st.GetOrCreateSource(gcal.SourceType, testAccount+"/primary")
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfig(src.ID, `{"account_email":"`+testAccount+`","calendar_id":"primary"}`))
	oldSyncID, err := st.StartSync(src.ID, "full")
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpoint(oldSyncID, &store.Checkpoint{
		PageToken: encodeCalendarFullCheckpoint(""), MessagesProcessed: 2, MessagesAdded: 2,
	}))

	_, err = s.Full(context.Background())
	require.NoError(err)

	// The old run was superseded (no longer 'running').
	var oldStatus string
	require.NoError(st.DB().QueryRow(
		st.Rebind("SELECT status FROM sync_runs WHERE id = ?"), oldSyncID).Scan(&oldStatus))
	assert.NotEqual(store.SyncStatusRunning, oldStatus, "resume must supersede the prior running run via StartSync")

	// The completed run's counter includes the 2 seeded + 1 newly ingested.
	var processed int64
	require.NoError(st.DB().QueryRow(st.Rebind(
		"SELECT messages_processed FROM sync_runs WHERE source_id = ? AND status = 'completed' ORDER BY id DESC LIMIT 1"),
		src.ID).Scan(&processed))
	assert.Equal(int64(3), processed, "resumed run seeds prior counters (2) + new (1)")
}

type listEventsRecorder struct {
	*gcal.MockAPI

	params     []gcal.EventsListParams
	failOnCall int
	err        error
}

func (r *listEventsRecorder) ListEvents(ctx context.Context, calendarID string, p gcal.EventsListParams) (*gcal.EventsPage, error) {
	r.params = append(r.params, p)
	if r.failOnCall > 0 && len(r.params) == r.failOnCall {
		return nil, r.err
	}
	return r.MockAPI.ListEvents(ctx, calendarID, p)
}
