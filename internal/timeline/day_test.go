package timeline

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

const singleDayEvent = `{"id":"planning","iCalUID":"planning@example.com","summary":"Planning","start":{"dateTime":"2026-10-22T10:00:00Z"},"end":{"dateTime":"2026-10-22T11:00:00Z"},"attendees":[{"email":"alice@example.com","displayName":"Alice"}]}`

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
func TestBuildDayUpcomingNext48h(t *testing.T) {
	f := newTimelineFixture(t)
	for i, at := range []string{"2026-10-23T11:00:00Z", "2026-10-24T11:00:00Z", "2026-10-24T13:00:00Z"} {
		f.calendar(strconv.Itoa(i), fmt.Sprintf(`{"id":%q,"start":{"dateTime":%q},"end":{"dateTime":%q}}`, strconv.Itoa(i), at, at))
	}
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(t, err)
	assert.Len(t, got.Upcoming, 2)
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
	transcripts := []Entry{}
	for _, e := range allDayEntries(got) {
		if e.Kind == "transcript" {
			transcripts = append(transcripts, e)
		}
	}
	requirements.Len(transcripts, 1)
	assertions.True(transcripts[0].HiddenText)
	assertions.Empty(transcripts[0].Meeting.Title.Text)
	req.Date = "2026-10-20"
	req.Days = 0
	old, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(allDayEntries(old), 1)
	assertions.True(allDayEntries(old)[0].HiddenText)
	assertions.Empty(allDayEntries(old)[0].Burst.Snippet.Text)
	requirements.NotNil(got.OpenLoops)
	requirements.Len(got.OpenLoops.Items, 1)
	assertions.Equal(fmt.Sprintf("msg:%d", email), got.OpenLoops.Items[0].Ref)
	assertions.True(got.OpenLoops.Items[0].HiddenText)
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
// disappear when fitting actual pages. One logical burst contains 6,000 received messages.
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
		err = tx.QueryRowContext(t.Context(), f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sent_at,sender_id,is_sent,source_is_from_me) VALUES(?,?,?,'email',?,?,TRUE,FALSE) RETURNING id`), f.Source.ID, f.ConvID, fmt.Sprintf("bulk%d", i), f.now.Add(time.Duration(i)*time.Millisecond), f.peer).Scan(&id)
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

func TestReadActivityReportsBurstCap(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineFixture(t)
	for i := range 3 {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("page-%d", i), "Synthetic conversation")
		requirements.NoError(err)
		f.add(f.now.Add(time.Duration(i)*time.Second), false, func(m *store.Message) { m.ConversationID = conv })
	}
	f.project()
	from, to := f.now.Add(-time.Hour), f.now.Add(time.Hour)
	for _, tc := range []struct {
		name  string
		query store.BurstQuery
	}{
		{"day", store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Filters: f.src.Filters, Limit: 2}},
		{"timestamp", store.BurstQuery{After: &from, Before: &to, Filters: f.src.Filters, Limit: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			c := composer{ctx: t.Context(), src: f.src, t: &Timeline{Window: Window{From: "2026-10-22", To: "2026-10-22"}}, loc: time.UTC}
			got, err := c.readActivity(tc.query)
			requirements.NoError(err)
			requirements.Len(got.bursts, 2)
			assertions.Equal([]int64{2}, got.bursts[1].LatestMessageIDs)
			assertions.True(got.more)
		})
	}
}

func TestBuildSummaryCountsLinkedPersonOnce(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	f.savePerson()
	sibling := f.EnsureParticipant("alice-work@example.com", "Alice Work", "example.com")
	_, err := f.Store.LinkParticipants(f.peer, sibling)
	requirements.NoError(err)
	at := f.now.AddDate(0, 0, -3)
	for i, participant := range []int64{f.peer, sibling} {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("summary-%d", i), "Synthetic conversation")
		requirements.NoError(err)
		id := f.add(at, false, func(m *store.Message) {
			m.ConversationID = conv
			m.SenderID.Int64 = participant
		})
		requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{participant}, nil))
	}
	f.project()

	req := dayRequest()
	req.Date, req.Days = "", 5
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.Days, 1)
	requirements.NotNil(got.Days[0].Summary)
	assertions.Equal(1, got.Days[0].Summary.PeopleCount)
}

func TestBuildDaySkippedMidnightKeepsHoursAndNote(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	loc, err := time.LoadLocation("America/Santiago")
	requirements.NoError(err)
	f.src.Location = loc
	f.src.Clock = func() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, loc) }
	for _, stamp := range []string{"2026-09-06T03:30:00Z", "2026-09-06T04:30:00Z", "2026-09-07T02:30:00Z", "2026-09-07T03:30:00Z"} {
		at, err := time.Parse(time.RFC3339, stamp)
		requirements.NoError(err)
		f.call(at)
	}
	_, err = f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-09-06", Body: "Sunday note", Author: "owner"})
	requirements.NoError(err)
	req := dayRequest()
	req.Date = "2026-09-06"
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	entries := allDayEntries(got)
	var calls []string
	var notes []string
	for _, entry := range entries {
		if entry.Kind == "call" {
			calls = append(calls, entry.Time.UTC().Format(time.RFC3339))
		}
		if entry.Kind == "note" {
			notes = append(notes, entry.Note.Text.Text)
		}
	}
	assertions.ElementsMatch([]string{"2026-09-06T04:30:00Z", "2026-09-07T02:30:00Z"}, calls)
	assertions.Equal([]string{"Sunday note"}, notes)
}
