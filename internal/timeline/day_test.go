package timeline

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const singleDayEvent = `{"id":"planning","iCalUID":"planning@example.com","summary":"Planning","start":{"dateTime":"2026-10-22T10:00:00Z"},"end":{"dateTime":"2026-10-22T11:00:00Z"},"attendees":[{"email":"alice@example.com","displayName":"Alice"}]}`

type blockingInventory struct{}

func (blockingInventory) Build(ctx context.Context, _ time.Time) (Freshness, error) {
	<-ctx.Done()
	return Freshness{}, ctx.Err()
}

func TestBuildDaySlowInventoryLeavesTimeForDisplayHydration(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	f.add(f.now.Add(-time.Hour), false)
	f.calendar("pg-lock", singleDayEvent)
	f.project()
	f.src.Inventory = blockingInventory{}

	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	entries := allDayEntries(got)
	var hasMeeting, hasBurst bool
	for _, entry := range entries {
		hasMeeting = hasMeeting || entry.Meeting != nil
		hasBurst = hasBurst || entry.Burst != nil
	}
	assertions.True(hasMeeting, "the calendar entry collected before the optional inventory timeout should remain visible")
	assertions.True(hasBurst, "the activity loaded before the optional inventory timeout must still be hydrated")
}

func TestBuildDaySlowSnapshotReadersLeaveTimeForDisplayHydration(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	f.add(f.now.Add(-time.Hour), false)
	f.project()
	f.src.Loops = blockingLoops{}
	f.src.Inventory = blockingInventory{}
	ctx, release, err := f.Store.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()

	got, err := Build(ctx, f.src, dayRequest())
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 1)
	assertions.NotNil(entries[0].Burst, "slow optional snapshot readers must leave time to hydrate the loaded activity")
}

type signalingCalendarSource struct {
	CalendarSource

	started chan struct{}
	once    sync.Once
}

func (s *signalingCalendarSource) ListCalendarOccurrencesContext(ctx context.Context, query store.CalendarOccurrenceQuery) (store.CalendarOccurrencePage, error) {
	s.once.Do(func() { close(s.started) })
	return s.CalendarSource.ListCalendarOccurrencesContext(ctx, query)
}

type pausingActivitySource struct {
	ActivitySource

	collected chan struct{}
	resume    <-chan struct{}
	once      sync.Once
	err       error
}

func (s *pausingActivitySource) ListActivityBurstsContext(ctx context.Context, query store.BurstQuery) (store.BurstPage, error) {
	page, err := s.ActivitySource.ListActivityBurstsContext(ctx, query)
	s.err = err
	s.once.Do(func() { close(s.collected) })
	select {
	case <-s.resume:
	case <-ctx.Done():
	}
	return page, err
}

func TestBuildDaySnapshotRecoversAfterCanceledPostgresCalendarQuery(t *testing.T) {
	if !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL query-cancellation regression requires MSGVAULT_TEST_DB")
	}
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	f.add(f.now.Add(-time.Hour), false)
	f.project()

	blocker, err := f.Store.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = blocker.Rollback() }()

	ctx, release, err := f.Store.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	reader := store.ReadDBContext(ctx, f.Store.DB())
	var backendPID int
	requirements.NoError(reader.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&backendPID))

	started := make(chan struct{})
	collected := make(chan struct{})
	continueRead := make(chan struct{})
	var continueOnce sync.Once
	resume := func() { continueOnce.Do(func() { close(continueRead) }) }
	defer resume()
	activity := &pausingActivitySource{ActivitySource: f.src.Activity, collected: collected, resume: continueRead}
	f.src.Activity = activity
	f.src.Calendar = &signalingCalendarSource{CalendarSource: f.src.Calendar, started: started}
	type buildResult struct {
		timeline *Timeline
		err      error
	}
	done := make(chan buildResult, 1)
	go func() {
		got, buildErr := Build(ctx, f.src, dayRequest())
		done <- buildResult{timeline: got, err: buildErr}
	}()

	select {
	case <-collected:
		requirements.NoError(activity.err)
	case result := <-done:
		requirements.FailNowf("timeline build finished before collecting activity", "error: %v", result.err)
	case <-time.After(10 * time.Second):
		stacktest.Dump(t)
		requirements.FailNow("activity reader did not finish before the collection deadline")
	}
	_, err = blocker.ExecContext(t.Context(), "LOCK TABLE calendar_event_attendees IN ACCESS EXCLUSIVE MODE")
	requirements.NoError(err)
	resume()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		stacktest.Dump(t)
		requirements.FailNow("calendar reader did not start")
	}
	requirements.Eventually(func() bool {
		var waitEventType sql.NullString
		queryErr := f.Store.DB().QueryRowContext(t.Context(), `SELECT wait_event_type FROM pg_stat_activity WHERE pid = $1`, backendPID).Scan(&waitEventType)
		return queryErr == nil && waitEventType.Valid && waitEventType.String == "Lock"
	}, 5*time.Second, 25*time.Millisecond, "the real calendar query should be waiting on the PostgreSQL table lock")

	// Wait until the collection deadline cancels the real query before
	// releasing the lock, so transaction recovery runs before display hydration.
	requirements.Eventually(func() bool {
		var state, waitEventType sql.NullString
		queryErr := f.Store.DB().QueryRowContext(t.Context(), `SELECT state, wait_event_type FROM pg_stat_activity WHERE pid = $1`, backendPID).Scan(&state, &waitEventType)
		if queryErr == sql.ErrNoRows {
			return true
		}
		return queryErr == nil && state.Valid && state.String != "active" && (!waitEventType.Valid || waitEventType.String != "Lock")
	}, 8*time.Second, 25*time.Millisecond, "the collection deadline should cancel the blocked PostgreSQL query")
	requirements.NoError(blocker.Rollback())

	var result buildResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		stacktest.Dump(t)
		requirements.FailNow("timeline build did not finish after releasing the PostgreSQL lock")
	}
	requirements.NoError(result.err)
	entries := allDayEntries(result.timeline)
	var hydratedBurst bool
	for _, entry := range entries {
		if entry.Burst != nil {
			hydratedBurst = true
		}
	}
	assertions.True(hydratedBurst, "activity loaded before the canceled calendar query must still be hydrated")
	var timedOut bool
	for _, gap := range result.timeline.Gaps {
		if gap.Section == "meetings" && gap.Code == "timeout" {
			timedOut = true
			break
		}
	}
	assertions.True(timedOut, "the canceled calendar query should be reported as a timeout")
}

func TestBuildDayMergesLinkedTranscriptIntoMeeting(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	event := f.calendar("planning", singleDayEvent)
	note := f.transcript("notes", "planning@example.com", f.now.Add(-2*time.Hour))
	f.project()
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 1)
	assertions.Equal("meeting", entries[0].Kind)
	assertions.Equal(fmt.Sprintf("evt:%d", event), entries[0].AnchorRef)
	assertions.True(entries[0].Meeting.Attended)
	assertions.Equal([]int64{note}, entries[0].Meeting.LinkedTranscriptIDs)
	assertions.Equal("Planning", entries[0].Meeting.Title.Text)
	requirements.NoError(entries[0].Validate())
}
func TestBuildDayKeepsLinkedOvernightMeetingInsideWindow(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	uid := "overnight@example.org"
	event := f.calendar("overnight", fmt.Sprintf(`{"id":"overnight","iCalUID":%q,"summary":"Overnight planning","start":{"dateTime":"2026-10-21T23:00:00Z"},"end":{"dateTime":"2026-10-22T13:00:00Z"}}`, uid))
	note := f.transcript("overnight-notes", uid, f.now.Add(-2*time.Hour))
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO meeting_link_decisions(member_message_id,target_ical_uid,occurrence_key,decision,created_at) VALUES (?,?,'','link',?)`), note, uid, f.now)
	requirements.NoError(err)
	f.project()

	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	requirements.Len(got.Days, 1)
	assertions.Equal("2026-10-22", got.Days[0].Date)
	entries := allDayEntries(got)
	requirements.Len(entries, 1)
	assertions.Equal("meeting", entries[0].Kind)
	assertions.Equal(fmt.Sprintf("evt:%d", event), entries[0].AnchorRef)
	assertions.Equal(time.Date(2026, 10, 21, 23, 0, 0, 0, time.UTC), entries[0].Meeting.Start)
	assertions.Equal([]int64{note}, entries[0].Meeting.LinkedTranscriptIDs)
}

func TestBuildDayOverlappingCallDoesNotSetAttended(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.calendar("planning", singleDayEvent)
	f.project()
	f.call(f.now.Add(-2 * time.Hour))
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 2)
	assertions.False(entries[0].Meeting.Attended)
	assertions.Equal("call", entries[1].Kind)
}

func TestBuildDayIncludesCallWithoutParticipantRoster(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.callWithoutRoster(f.now.Add(-time.Hour))

	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 1)
	assertions.Equal("call", entries[0].Kind)
	requirements.NotNil(entries[0].Call)
	assertions.Equal("unknown", entries[0].Call.Outcome)
	assertions.Zero(entries[0].Call.PeopleCount)
	assertions.Empty(entries[0].Call.People)
}

func TestBuildDayPreservesDateOnlyCallDateInVancouver(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	loc, err := time.LoadLocation("America/Vancouver")
	requirements.NoError(err)
	f.src.Location = loc
	f.dayCall(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC))

	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	requirements.Len(got.Days, 1)
	assertions.Equal("2026-10-22", got.Days[0].Date)
	requirements.Len(got.Days[0].Calls, 1)
	assertions.Equal("day", got.Days[0].Calls[0].Call.Precision)
	assertions.Equal("2026-10-22", got.Days[0].Calls[0].Call.Date)
	assertions.Contains(Markdown(got), "2026-10-22 call")
	assertions.NotContains(Markdown(got), "17:00 call")
}
func TestBuildDayRecurringSeriesMovedOccurrence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.calendar("weekly", `{"id":"weekly","start":{"dateTime":"2026-10-15T10:00:00Z"},"end":{"dateTime":"2026-10-15T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	moved := f.calendar("moved", `{"id":"moved","recurringEventId":"weekly","originalStartTime":{"dateTime":"2026-10-22T10:00:00Z"},"start":{"dateTime":"2026-10-22T15:00:00Z"},"end":{"dateTime":"2026-10-22T16:00:00Z"}}`)
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	requirements.Len(allDayEntries(got), 1)
	e := allDayEntries(got)[0]
	assertions.Equal(fmt.Sprintf("evt:%d", moved), e.Ref)
	assertions.Equal(15, e.Time.Hour())
}
func TestBuildDayDraftReplyNotOutbound(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.add(f.now.Add(-time.Hour), false)
	draft := f.add(f.now, true)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), draft)
	requirements.NoError(err)
	f.project()
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	requirements.Len(allDayEntries(got), 1)
	assertions.Equal(1, allDayEntries(got)[0].Burst.Inbound)
	assertions.Zero(allDayEntries(got)[0].Burst.Outbound)
}
func TestBuildDayImportedOldMessageLandsOnSentDate(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.add(time.Date(2019, 10, 22, 12, 0, 0, 0, time.UTC), false)
	f.project()
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	assertions.Empty(allDayEntries(got))
	req := dayRequest()
	req.Date = "2019-10-22"
	old, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(allDayEntries(old), 1)
	assertions.Equal("2019-10-22", old.Days[0].Date)
}
func TestBuildDayRanksTiersThenExchanges(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	person := f.savePerson()
	f.tier(person, 1)
	first := f.add(f.now.Add(-3*time.Hour), false)
	exchange, err := f.Store.EnsureConversation(f.Source.ID, "exchange", "Exchange")
	requirements.NoError(err)
	f.add(f.now.Add(-2*time.Hour), false, func(m *store.Message) { m.ConversationID = exchange })
	f.add(f.now.Add(-time.Hour), true, func(m *store.Message) { m.ConversationID = exchange })
	newer, err := f.Store.EnsureConversation(f.Source.ID, "unsaved", "Unsaved")
	requirements.NoError(err)
	other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	id := f.add(f.now, false, func(m *store.Message) {
		m.ConversationID = newer
		m.SenderID = sql.NullInt64{Int64: other, Valid: true}
	})
	requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{other}, nil))
	f.project()
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 3)
	assertions.Equal(fmt.Sprintf("conv:%d", exchange), entries[0].Ref)
	assertions.Equal([]int64{first}, entries[1].Burst.LatestMessageIDs)
	assertions.Equal(fmt.Sprintf("conv:%d", newer), entries[2].Ref)
}
func TestBuildDayTierFilterDropsUnsaved(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now, false)
	f.project()
	req := dayRequest()
	req.Tiers = []int{1, 2}
	got, err := Build(t.Context(), f.src, req)
	require.NoError(t, err)
	assert.Empty(t, allDayEntries(got))
}
func TestBuildDayOlderDaysSummarized(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.add(f.now.AddDate(0, 0, -2), false)
	f.project()
	req := dayRequest()
	req.Date = ""
	req.Days = 3
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.Days, 1)
	requirements.NotNil(got.Days[0].Summary)
	assertions.Equal("2026-10-20", got.Days[0].Date)
	assertions.Equal(1, got.Days[0].Summary.PeopleCount)
}

type blockingLoops struct{}

func (blockingLoops) List(ctx context.Context, _ loops.Options, _ time.Time) (loops.Result, error) {
	<-ctx.Done()
	return loops.Result{}, ctx.Err()
}
func TestBuildDayLoopsTimeoutIsRetryableGap(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now, false)
	f.project()
	f.src.Loops = blockingLoops{}
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(t, err)
	assert.Contains(t, got.Gaps, Gap{Section: "loops", Code: "timeout", Retryable: true})
	assert.Len(t, allDayEntries(got), 1)
}
func TestBuildDayIncludeColdMergesByMessageID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	src, err := f.Store.GetOrCreateSource("beeper", "example-account")
	requirements.NoError(err)
	conv, err := f.Store.EnsureConversation(src.ID, "quiet", "Quiet")
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='direct_chat' WHERE id=?`), conv)
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	id := f.add(f.now, false, func(m *store.Message) { m.SourceID = src.ID; m.ConversationID = conv; m.MessageType = "chat" })
	requirements.NoError(f.seedConversationPolicy(t.Context(), src.ID, "quiet"))
	f.project()
	req := dayRequest()
	req.IncludeCold = true
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(allDayEntries(got), 1)
	assertions.Equal([]int64{id}, allDayEntries(got)[0].Burst.LatestMessageIDs)
	req.IncludeCold = false
	hidden, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	assertions.Empty(allDayEntries(hidden))
	assertions.Equal(1, hidden.Omitted.Cold)
}

func TestBuildDayIncludeColdCollapsesCanonicalEmailCopies(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	warmID := f.PersistEmail(storetest.EmailSpec{Thread: "warm-copy", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "Shared message", Body: "Synthetic copy", SentAt: f.now.Add(-time.Hour)})
	coldID := f.PersistEmail(storetest.EmailSpec{Thread: "cold-copy", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "Shared message", Body: "Synthetic copy", SentAt: f.now.Add(-time.Hour)})
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET rfc822_message_id=? WHERE id IN (?,?)`), "<shared@example.test>", warmID, coldID)
	requirements.NoError(err)
	requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, "cold-copy"))
	f.project()

	req := dayRequest()
	req.IncludeCold = true
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 1)
	assertions.Equal(1, entries[0].Burst.Inbound)
	assertions.Len(entries[0].Burst.LatestMessageIDs, 1)
	assertions.Equal(2, entries[0].Burst.Copies[entries[0].Burst.LatestMessageIDs[0]])
}

func TestBuildIncludesColdMeetingTranscriptsAcrossModes(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineFixture(t)
	const key = "cold-meeting"
	id := f.transcript(key, "", f.now.Add(-time.Hour))
	source, err := f.Store.GetOrCreateSource("meeting_import", "notes@example.com")
	requirements.NoError(err)
	requirements.NoError(f.seedConversationPolicy(t.Context(), source.ID, key))
	f.project()
	displays, err := f.Store.MessageDisplayContext(t.Context(), []int64{id})
	requirements.NoError(err)
	conversationID := displays[id].ConversationID

	cases := []struct {
		name    string
		request Request
		entries func(*Timeline) []Entry
		anchor  bool
	}{
		{name: "day", request: dayRequest(), entries: allDayEntries},
		{name: "relationship", request: relationshipRequest("email:alice@example.com"), entries: func(got *Timeline) []Entry { return got.History }},
		{name: "message anchor", request: anchorRequest(messageRef(id)), entries: func(got *Timeline) []Entry { return got.History }, anchor: true},
		{name: "conversation anchor", request: anchorRequest(fmt.Sprintf("conv:%d", conversationID)), entries: func(got *Timeline) []Entry { return got.History }, anchor: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			req := tc.request
			req.IncludeCold = true
			req.IncludeNoise = true
			got, err := Build(t.Context(), f.src, req)
			requirements.NoError(err)
			entries := tc.entries(got)
			requirements.Len(entries, 1)
			assertions.Equal("transcript", entries[0].Kind)
			assertions.Equal(messageRef(id), entries[0].Ref)
			assertions.Equal(tc.anchor, entries[0].Anchor)
			assertions.Zero(got.Omitted.Cold)
		})
	}
}

func TestBuildReappliesNoiseAfterColdCopyMergeAcrossModes(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineFixture(t)
	warmID := f.PersistEmail(storetest.EmailSpec{Thread: "warm-copy", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "Shared message", Body: "Synthetic copy", SentAt: f.now.Add(-time.Hour)})
	coldID := f.PersistEmail(storetest.EmailSpec{Thread: "cold-copy", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "Shared message", Body: "Synthetic copy", SentAt: f.now.Add(-time.Hour)})
	warmListID := f.PersistEmail(storetest.EmailSpec{Thread: "warm-copy", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "List update", Body: "Synthetic list message", SentAt: f.now.Add(-2 * time.Hour)})
	coldListID := f.PersistEmail(storetest.EmailSpec{Thread: "cold-copy", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "List update", Body: "Synthetic list message", SentAt: f.now.Add(-2 * time.Hour)})
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET rfc822_message_id=? WHERE id IN (?,?)`), "<shared@example.test>", warmID, coldID)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='updates.example.test' WHERE id IN (?,?)`), warmListID, coldListID)
	requirements.NoError(err)
	requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, "cold-copy"))
	f.src.Filters.Exclusions.ExcludeListMessages = true
	f.project()

	cases := []struct {
		name    string
		request Request
		entries func(*Timeline) []Entry
	}{
		{name: "day", request: dayRequest(), entries: allDayEntries},
		{name: "relationship", request: relationshipRequest("email:alice@example.com"), entries: func(got *Timeline) []Entry { return got.History }},
		{name: "anchor", request: anchorRequest(messageRef(warmID)), entries: func(got *Timeline) []Entry { return got.History }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			req := tc.request
			req.IncludeCold = true
			got, err := Build(t.Context(), f.src, req)
			requirements.NoError(err)
			requirements.Len(tc.entries(got), 1)
			assertions.Equal(1, got.Omitted.Noise)

			req.IncludeNoise = true
			got, err = Build(t.Context(), f.src, req)
			requirements.NoError(err)
			assertions.Len(tc.entries(got), 2)
			assertions.Zero(got.Omitted.Noise)
		})
	}
}

func TestBuildDayUpcomingNext48h(t *testing.T) {
	f := newTimelineFixture(t)
	for i, at := range []string{"2026-10-23T11:00:00Z", "2026-10-24T11:00:00Z", "2026-10-24T13:00:00Z"} {
		f.calendar(strconv.Itoa(i), fmt.Sprintf(`{"id":%q,"start":{"dateTime":%q},"end":{"dateTime":%q}}`, strconv.Itoa(i), at, at))
	}
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(t, err)
	assert.Len(t, got.Upcoming, 2)
}

func TestBuildDayUpcomingIncludesAllDayEventAcrossTimezoneDateBoundary(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	location, err := time.LoadLocation("Asia/Tokyo")
	requirements.NoError(err)
	f.src.Location = location
	f.now = time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC)
	f.src.Clock = func() time.Time { return f.now }
	f.calendar("tokyo-all-day", `{"id":"tokyo-all-day","summary":"Tokyo planning","start":{"date":"2026-10-24"},"end":{"date":"2026-10-25"}}`)

	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	requirements.Len(got.Upcoming, 1)
	assertions.Equal("Tokyo planning", got.Upcoming[0].Meeting.Title.Text)
}

func TestBuildDaySubjectFilterAppliesToEntriesAndUpcoming(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	aliceMessage := f.PersistEmail(storetest.EmailSpec{Thread: "alice-thread", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "Alice update", Body: "Synthetic Alice message", SentAt: f.now.Add(-time.Hour)})
	f.PersistEmail(storetest.EmailSpec{Thread: "bob-thread", From: "bob@example.com", To: []string{storetest.OwnerAddress}, Subject: "Bob update", Body: "Synthetic Bob message", SentAt: f.now.Add(-2 * time.Hour)})
	f.calendar("alice-meeting", `{"id":"alice-meeting","summary":"Alice planning","start":{"dateTime":"2026-10-22T13:00:00Z"},"end":{"dateTime":"2026-10-22T14:00:00Z"},"attendees":[{"email":"alice@example.com","displayName":"Alice"}]}`)
	f.calendar("bob-meeting", `{"id":"bob-meeting","summary":"Bob planning","start":{"dateTime":"2026-10-22T14:00:00Z"},"end":{"dateTime":"2026-10-22T15:00:00Z"},"attendees":[{"email":"bob@example.com","displayName":"Bob"}]}`)
	person := f.savePerson()
	f.project()

	req := dayRequest()
	req.Mode = ModeDay
	req.With = fmt.Sprintf("person:%d", person)
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)

	entries := allDayEntries(got)
	var messageIDs []int64
	var meetingTitles []string
	for _, entry := range entries {
		if entry.Burst != nil {
			messageIDs = append(messageIDs, entry.Burst.LatestMessageIDs...)
		}
		if entry.Kind == "meeting" && entry.Meeting != nil {
			meetingTitles = append(meetingTitles, entry.Meeting.Title.Text)
		}
	}
	assertions.Equal([]int64{aliceMessage}, messageIDs)
	assertions.Equal([]string{"Alice planning"}, meetingTitles)
	requirements.Len(got.Upcoming, 1)
	assertions.Equal("Alice planning", got.Upcoming[0].Meeting.Title.Text)
}

func TestBuildDaySubjectFiltersOpenLoopsBeforeLimit(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	person := f.savePerson()
	for i := range 11 {
		f.PersistEmail(storetest.EmailSpec{
			Thread: fmt.Sprintf("unrelated-loop-%02d", i), From: storetest.OwnerAddress,
			To: []string{"bob@example.com"}, Subject: "Contract?", Body: "Can you send the contract?",
			SentAt: f.now.AddDate(0, 0, i-21),
		})
	}
	bobParticipant := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	bobPerson, _, err := f.Store.CreatePersonFromParticipant(bobParticipant)
	requirements.NoError(err)
	assertions.NotEqual(person, bobPerson.ID)
	selected := f.PersistEmail(storetest.EmailSpec{
		Thread: "selected-loop", From: storetest.OwnerAddress,
		To: []string{"alice@example.com"}, Subject: "Contract?", Body: "Can you send the contract?",
		SentAt: f.now.AddDate(0, 0, -5),
	})
	allLoops, err := f.src.Loops.List(t.Context(), loops.Options{Limit: 10}, f.now)
	requirements.NoError(err)
	requirements.Len(allLoops.Loops, 10)
	assertions.True(allLoops.Truncated)
	for _, loop := range allLoops.Loops {
		assertions.NotEqual(selected, loop.Anchor.ID)
	}
	sourceLoops, err := f.src.Loops.List(t.Context(), loops.Options{PersonID: person, Limit: 10}, f.now)
	requirements.NoError(err)
	requirements.Len(sourceLoops.Loops, 1)

	req := dayRequest()
	req.Mode = ModeDay
	req.With = fmt.Sprintf("person:%d", person)
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.OpenLoops)
	requirements.Len(got.OpenLoops.Items, 1)
	assertions.Equal(fmt.Sprintf("msg:%d", selected), got.OpenLoops.Items[0].AnchorRef)
	requirements.NotNil(got.OpenLoops.Items[0].Loop)
	assertions.Equal(person, got.OpenLoops.Items[0].Loop.Person.PersonID)
}

func TestBuildDayUnsavedSubjectFiltersOpenLoopsBeforeLimit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	for i := range 11 {
		f.PersistEmail(storetest.EmailSpec{
			Thread: fmt.Sprintf("unrelated-unsaved-loop-%02d", i), From: storetest.OwnerAddress,
			To: []string{"bob@example.com"}, Subject: "Contract?", Body: "Can you send the contract?",
			SentAt: f.now.AddDate(0, 0, i-21),
		})
	}
	selected := f.PersistEmail(storetest.EmailSpec{
		Thread: "selected-unsaved-loop", From: storetest.OwnerAddress,
		To: []string{"alice@example.com"}, Subject: "Contract?", Body: "Can you send the contract?",
		SentAt: f.now.AddDate(0, 0, -5),
	})
	f.project()

	req := dayRequest()
	req.Mode, req.With = ModeDay, "email:alice@example.com"
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.OpenLoops)
	requirements.Len(got.OpenLoops.Items, 1)
	assertions.Equal(messageRef(selected), got.OpenLoops.Items[0].AnchorRef)
	requirements.NotNil(got.OpenLoops.Items[0].Loop)
	assertions.Equal(f.peer, got.OpenLoops.Items[0].Loop.Person.ParticipantID)
}

func TestBuildDayPreservesCalendarCancellationAndExcludesCancelledUpcoming(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.calendar("past-cancelled", `{"id":"past-cancelled","summary":"Past cancellation","status":"cancelled","start":{"dateTime":"2026-10-22T10:00:00Z"},"end":{"dateTime":"2026-10-22T10:30:00Z"}}`)
	f.calendar("future-cancelled", `{"id":"future-cancelled","summary":"Future cancellation","status":"cancelled","start":{"dateTime":"2026-10-22T13:00:00Z"},"end":{"dateTime":"2026-10-22T13:30:00Z"}}`)
	f.calendar("future-deleted", `{"id":"future-deleted","summary":"Future source deletion","start":{"dateTime":"2026-10-22T13:30:00Z"},"end":{"dateTime":"2026-10-22T14:00:00Z"}}`)
	f.calendar("future-active", `{"id":"future-active","summary":"Active planning","start":{"dateTime":"2026-10-22T14:00:00Z"},"end":{"dateTime":"2026-10-22T14:30:00Z"}}`)
	calendarSource, err := f.Store.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.MarkMessagesDeletedBatch(calendarSource.ID, []string{"future-deleted"}))
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)

	statuses := map[string]string{}
	for _, entry := range allDayEntries(got) {
		if entry.Kind == "meeting" && entry.Meeting != nil {
			statuses[entry.Meeting.Title.Text] = entry.Meeting.Status
		}
	}
	assertions.Equal("cancelled", statuses["Past cancellation"])
	assertions.Equal("cancelled", statuses["Future cancellation"])
	var deletedEvent *MeetingEntry
	for _, entry := range allDayEntries(got) {
		if entry.Meeting != nil && entry.Meeting.Title.Text == "Future source deletion" {
			deletedEvent = entry.Meeting
		}
	}
	requirements.NotNil(deletedEvent)
	assertions.True(deletedEvent.DeletedFromSource)
	requirements.Len(got.Upcoming, 1)
	assertions.Equal("Active planning", got.Upcoming[0].Meeting.Title.Text)
	assertions.Contains(Markdown(got), "Past cancellation (cancelled)")
	assertions.Contains(Markdown(got), "Future cancellation (cancelled)")
	assertions.Contains(Markdown(got), "Future source deletion (deleted from source)")
}

func TestBuildDayAllDayEventUsesActivityZoneForBothBounds(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	f.src.Location = time.FixedZone("TestPlusNine", 9*60*60)
	f.calendar("all-day", `{"id":"all-day","summary":"All day planning","start":{"date":"2026-10-22"},"end":{"date":"2026-10-23"}}`)

	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	var meeting *MeetingEntry
	for _, entry := range allDayEntries(got) {
		if entry.Meeting != nil && entry.Meeting.Title.Text == "All day planning" {
			meeting = entry.Meeting
		}
	}
	requirements.NotNil(meeting)
	assertions.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, f.src.Location), meeting.Start)
	assertions.Equal(time.Date(2026, 10, 23, 0, 0, 0, 0, f.src.Location), meeting.End)
	assertions.True(meeting.End.After(meeting.Start))
}
func TestBuildDayReportsUnexpandableSeries(t *testing.T) {
	f := newTimelineFixture(t)
	event := f.calendar("bad", `{"id":"bad","start":{"date":"2026-10-22"},"end":{"date":"2026-10-23"},"recurrence":["RRULE:FREQ=INVALID"]}`)
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(t, err)
	var found bool
	for _, gap := range got.Gaps {
		if gap.Section == "meetings" && gap.Code == "partial" {
			found = true
			assert.Contains(t, gap.Items, fmt.Sprintf("evt:%d", event))
		}
	}
	assert.True(t, found)
}
func TestBuildEmptyWindowRendersExplicitNothingLine(t *testing.T) {
	assertions := assert.New(t)
	f := newTimelineFixture(t)
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(t, err)
	assertions.Contains(Markdown(got), "nothing archived")
	assertions.Contains(Markdown(got), "UTC (set [activity].timezone)")
	assertions.False(got.Now.IsZero())
	assertions.Equal(f.now, got.Now)
}
func TestBuildDSTDayGroupsOccurrencesByLocalStart(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	loc, err := time.LoadLocation("America/New_York")
	requirements.NoError(err)
	f.src.Location = loc
	f.add(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), false)
	f.add(time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), false)
	f.calendar("dst", `{"id":"dst","start":{"dateTime":"2026-11-02T03:30:00Z"},"end":{"dateTime":"2026-11-02T04:00:00Z"}}`)
	f.project()
	req := dayRequest()
	req.Date = "2026-11-01"
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.Days, 1)
	assertions.Equal("2026-11-01", got.Days[0].Date)
	assertions.Len(got.Days[0].Meetings, 1)
	requirements.Len(got.Days[0].Bursts, 1)
	assertions.Equal(2, got.Days[0].Bursts[0].Burst.Inbound)
}
func TestBuildHiddenTextReachesLoopsTranscriptsAndUnsavedEmail(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	email := f.PersistEmail(storetest.EmailSpec{Thread: "hidden-loop", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "Synthetic message", Body: "Can you send the contract?", SentAt: f.now.Add(-48 * time.Hour)})
	note := f.transcript("notes", "", f.now.Add(-time.Hour))
	for _, id := range []int64{email, note} {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET hidden_text_present=TRUE WHERE id=?`), id)
		requirements.NoError(err)
	}
	f.project()
	req := dayRequest()
	req.Date = ""
	req.Days = 3
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	for _, e := range allDayEntries(got) {
		if e.Kind == "transcript" {
			assertions.True(e.HiddenText)
			assertions.Empty(e.Meeting.Title.Text)
		}
	}
	req.Date = "2026-10-20"
	req.Days = 0
	old, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(allDayEntries(old), 1)
	assertions.True(allDayEntries(old)[0].HiddenText)
	assertions.Empty(allDayEntries(old)[0].Burst.Snippet.Text)
	requirements.NotNil(got.OpenLoops)
	requirements.NotEmpty(got.OpenLoops.Items)
	for _, e := range got.OpenLoops.Items {
		if e.Ref == fmt.Sprintf("msg:%d", email) {
			assertions.True(e.HiddenText)
		}
	}
	encoded, err := json.Marshal(got)
	requirements.NoError(err)
	assertions.NotContains(string(encoded), "Synthetic meeting")
	assertions.NotContains(string(encoded), "Synthetic message")
}
func TestBuildPinsClockAndWindowAcrossMidnight(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	for i := range 12 {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("pin%d", i), "Pinned")
		requirements.NoError(err)
		f.add(f.now.Add(time.Duration(i)*time.Millisecond), false, func(m *store.Message) { m.ConversationID = conv })
	}
	f.project()
	clockCalls := 0
	f.src.Clock = func() time.Time { clockCalls++; return f.now.In(time.FixedZone("test-west", -7*60*60)) }
	req := dayRequest()
	req.Date = ""
	req.Budget = 2048
	req.Format = "json"
	first, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotEmpty(first.Cursor)
	assertions.Equal(1, clockCalls)
	f.src.Clock = func() time.Time { return f.now.AddDate(0, 0, 1) }
	req.Cursor = first.Cursor
	next, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	assertions.Equal(first.Now, next.Now)
	assertions.Equal(first.Window, next.Window)
	assertions.NotEqual(allDayEntries(first)[0].Key, allDayEntries(next)[0].Key)
}
func TestBuildRejectsInvalidRequests(t *testing.T) {
	f := newTimelineFixture(t)
	for _, req := range []Request{{Days: 32}, {Before: 51}, {After: -1}, {Budget: 2000}, {Mode: "invalid"}, {Date: "bad"}, {Tiers: []int{4}}, {Date: "2026-10-22", Days: 4}, {From: "2026-10-22"}} {
		req.Scope.Owner = true
		_, err := Build(t.Context(), f.src, req)
		assert.Error(t, err)
	}
}

type fixtureLoops struct{ result loops.Result }

func (s fixtureLoops) List(context.Context, loops.Options, time.Time) (loops.Result, error) {
	return s.result, nil
}

type fixtureBursts struct {
	ActivitySource

	page store.BurstPage
}

func (s fixtureBursts) ListActivityBurstsContext(context.Context, store.BurstQuery) (store.BurstPage, error) {
	return s.page, nil
}

// Unequal entry sizes and tied ranks must never cause an entry to repeat or
// disappear when fitting actual pages. One logical burst contains 6,000 messages.
func TestBuildTraversesAllCursorsWithoutLossOrDuplicates(t *testing.T) {
	assertions := assert.New(t)
	requirements :=

		// Keep pagination independent of loop detection throughput on the bulk
		// message fixture. The real adapter and its deadline have separate tests.
		require.New(t)
	f := newTimelineFixture(t)

	f.PersistEmail(storetest.EmailSpec{Thread: "pending", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "Pending question", Body: "Can you send the contract?", SentAt: f.now.Add(-48 * time.Hour)})
	loopResult, err := f.src.Loops.List(t.Context(), loops.Options{Limit: 10}, f.now)
	requirements.NoError(err)
	requirements.NotEmpty(loopResult.Loops)
	f.src.Loops = fixtureLoops{loopResult}
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = tx.Rollback() }()
	for i := range 6000 {
		var id int64
		err = tx.QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at,sender_id,is_sent) VALUES(?,?,?,'email',?,?,TRUE) RETURNING id`), f.Source.ID, f.ConvID, fmt.Sprintf("bulk%d", i), f.now.Add(time.Duration(i)*time.Millisecond), f.peer).Scan(&id)
		requirements.NoError(err)
		_, err = tx.ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')`), id, f.owner)
		requirements.NoError(err)
	}
	requirements.NoError(tx.Commit())
	for i := range 22 {
		conv, e := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("page%d", i), "Paging")
		requirements.NoError(e)
		f.add(f.now.Add(time.Duration(i)*time.Second), i%2 == 0, func(m *store.Message) {
			m.ConversationID = conv
			m.Snippet = sql.NullString{String: strings.Repeat("界", i*4+1), Valid: true}
		})
	}
	f.transcript("page-note", "", f.now.Add(-2*time.Hour))
	f.calendar("planning", singleDayEvent)
	// This future occurrence appears in both pageable sections with one identity.
	f.calendar("future", `{"id":"future","summary":"Later today","start":{"dateTime":"2026-10-22T15:00:00Z"},"end":{"dateTime":"2026-10-22T16:00:00Z"}}`)
	_, err = f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-10-22", Body: "Authored note", Author: "owner"})
	requirements.NoError(err)
	copySource, e := f.Store.GetOrCreateSource("gmail", "second@example.com")
	requirements.NoError(e)
	requirements.NoError(f.Store.AddAccountIdentity(copySource.ID, "owner@example.com", "manual"))
	copyConv, e := f.Store.EnsureConversation(copySource.ID, "copy-thread", "Copy")
	requirements.NoError(e)
	for _, source := range []int64{f.Source.ID, copySource.ID} {
		f.add(f.now, false, func(m *store.Message) {
			m.SourceID = source
			if source == copySource.ID {
				m.ConversationID = copyConv
			}
			m.RFC822MessageID = sql.NullString{String: "<copy@example.com>", Valid: true}
		})
	}
	f.project()
	f.call(f.now.Add(-time.Hour))
	// Capture the actual reader result without Build's request deadline. This
	// test exercises continuation, independently of SQL throughput on the runner;
	// every other activity method still uses the real store adapter.
	realActivity := f.src.Activity
	burstQuery := store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Filters: f.src.Filters, Limit: maxBursts}
	burstPage, err := realActivity.ListActivityBurstsContext(t.Context(), burstQuery)
	requirements.NoError(err)
	requirements.Nil(burstPage.NextKey)
	var bulkGroups int
	for _, burst := range burstPage.Bursts {
		if burst.SourceID == f.Source.ID && burst.ConversationID == f.ConvID {
			bulkGroups++
			assertions.Equal(6001, burst.Inbound)
			assertions.Zero(burst.Outbound)
		}
	}
	requirements.Equal(1, bulkGroups)
	f.src.Activity = fixtureBursts{ActivitySource: realActivity, page: burstPage}
	req := dayRequest()
	req.Format = "markdown"
	full, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Empty(full.Cursor)
	requirements.NotEmpty(full.Upcoming)
	requirements.NotNil(full.OpenLoops)
	requirements.NotEmpty(full.OpenLoops.Items)
	var noteFound, copiesFound bool
	for _, entry := range allDayEntries(full) {
		if entry.Note != nil {
			noteFound = true
		}
		if entry.Burst != nil {
			for _, n := range entry.Burst.Copies {
				if n == 2 {
					copiesFound = true
				}
			}
		}
	}
	assertions.True(noteFound)
	assertions.True(copiesFound)
	expected := map[string]bool{}
	for _, e := range allDayEntries(full) {
		expected[e.Key] = true
		if e.Kind == "burst" && e.Ref == fmt.Sprintf("conv:%d", f.ConvID) {
			assertions.Equal(6001, e.Burst.Inbound)
		}
	}
	req.Budget = 2048
	seenUpcoming := map[string]bool{}
	seen := map[string]bool{}
	for range 80 {
		got, e := Build(t.Context(), f.src, req)
		requirements.NoError(e)
		assertions.LessOrEqual(len(Markdown(got)), 2048)
		entries := allDayEntries(got)
		for _, entry := range got.Upcoming {
			assertions.False(seenUpcoming[entry.Key])
			seenUpcoming[entry.Key] = true
		}
		for _, entry := range entries {
			assertions.False(seen[entry.Key], "duplicate %s", entry.Key)
			seen[entry.Key] = true
		}
		if got.Cursor == "" {
			break
		}
		requirements.Positive(len(entries) + len(got.Upcoming))
		req.Cursor = got.Cursor
	}
	assertions.Equal(expected, seen)
	assertions.Len(seenUpcoming, len(full.Upcoming))
	// Refresh the actual reader input after a later sync. Its new entry may appear
	// on the remaining side of the cursor; emitted keys must never repeat.
	req.Cursor = ""
	first, e := Build(t.Context(), f.src, req)
	requirements.NoError(e)
	requirements.NotEmpty(first.Cursor)
	insertedConv, e := f.Store.EnsureConversation(f.Source.ID, "inserted", "Inserted")
	requirements.NoError(e)
	insertedID := f.add(f.now.Add(-3*time.Hour), false, func(m *store.Message) { m.ConversationID = insertedConv })
	p, e := activity.NewProjector(f.Store, activity.Options{Timezone: f.src.Location.String()})
	requirements.NoError(e)
	_, e = p.ProjectMessages(t.Context(), []int64{insertedID})
	requirements.NoError(e)
	updatedPage, e := realActivity.ListActivityBurstsContext(t.Context(), burstQuery)
	requirements.NoError(e)
	requirements.Nil(updatedPage.NextKey)
	var insertedFound bool
	for _, burst := range updatedPage.Bursts {
		insertedFound = insertedFound || burst.ConversationID == insertedConv
	}
	requirements.True(insertedFound, "later source input must include the concurrent insert")
	f.src.Activity = fixtureBursts{ActivitySource: realActivity, page: updatedPage}
	seen = map[string]bool{}
	for _, entry := range allDayEntries(first) {
		seen[entry.Key] = true
	}
	req.Cursor = first.Cursor
	for range 80 {
		got, e := Build(t.Context(), f.src, req)
		requirements.NoError(e)
		for _, entry := range allDayEntries(got) {
			assertions.False(seen[entry.Key])
			seen[entry.Key] = true
		}
		if got.Cursor == "" {
			break
		}
		req.Cursor = got.Cursor
	}
	for key := range expected {
		assertions.True(seen[key], "lost %s", key)
	}
}

func TestBuildLoopsRetainsSettlementFromSourceDeletedReply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	question := f.PersistEmail(storetest.EmailSpec{Thread: "source-deleted-reply", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "Schedule", Body: "Can you send the schedule?", SentAt: f.now.Add(-48 * time.Hour)})
	f.project()
	req := dayRequest()
	before, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(before.OpenLoops)
	requirements.Len(before.OpenLoops.Items, 1)
	assertions.Equal(messageRef(question), before.OpenLoops.Items[0].Ref)
	reply := f.PersistEmail(storetest.EmailSpec{Thread: "source-deleted-reply", From: storetest.OwnerAddress, To: []string{"alice@example.com"}, Subject: "Schedule", Body: "The schedule is attached to the project page. We meet at noon.", SentAt: f.now.Add(-time.Hour)})
	message, err := f.Store.GetMessage(reply)
	requirements.NoError(err)
	requirements.NoError(f.Store.MarkMessageDeleted(message.SourceID, message.SourceMessageID))
	f.project()
	after, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(after.OpenLoops)
	assertions.Empty(after.OpenLoops.Items)
	for _, entry := range allDayEntries(after) {
		if entry.Burst != nil {
			assertions.NotContains(entry.Burst.LatestMessageIDs, reply)
		}
	}
}

func TestBuildLoopsRetainsSourceDeletedQuestionAnchor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	question := f.PersistEmail(storetest.EmailSpec{Thread: "source-deleted-question", From: "alice@example.com", To: []string{storetest.OwnerAddress}, Subject: "Schedule", Body: "Can you send the schedule?", SentAt: f.now.Add(-48 * time.Hour)})
	f.project()
	message, err := f.Store.GetMessage(question)
	requirements.NoError(err)
	requirements.NoError(f.Store.MarkMessageDeleted(message.SourceID, message.SourceMessageID))

	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	requirements.NotNil(got.OpenLoops)
	requirements.Len(got.OpenLoops.Items, 1)
	assertions.Equal(messageRef(question), got.OpenLoops.Items[0].Ref)
}
