package timeline

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func anchorRequest(ref string) Request {
	return Request{Around: ref, Scope: Scope{Owner: true}, Format: "json", Budget: 65536}
}

type anchorBurstRead struct {
	query store.BurstQuery
	page  store.BurstPage
}

type anchorBurstSnapshot struct {
	ActivitySource

	contactQuery *store.ContactQuery
	contact      store.ContactAggregate
	reads        []anchorBurstRead
	restricted   bool
	sourceIDs    []int64
}

func (s anchorBurstSnapshot) ListActivityBurstsContext(ctx context.Context, q store.BurstQuery) (store.BurstPage, error) {
	if sourcescope.Restricted(ctx) != s.restricted || !slices.Equal(sourcescope.IDs(ctx), s.sourceIDs) {
		return s.ActivitySource.ListActivityBurstsContext(ctx, q)
	}
	for _, read := range s.reads {
		if reflect.DeepEqual(read.query, q) {
			return read.page, nil
		}
	}
	return s.ActivitySource.ListActivityBurstsContext(ctx, q)
}

func (s anchorBurstSnapshot) MessageContactAggregateContext(ctx context.Context, q store.ContactQuery) (store.ContactAggregate, error) {
	if s.contactQuery != nil && reflect.DeepEqual(*s.contactQuery, q) && sourcescope.Restricted(ctx) == s.restricted && slices.Equal(sourcescope.IDs(ctx), s.sourceIDs) {
		return s.contact, nil
	}
	return s.ActivitySource.MessageContactAggregateContext(ctx, q)
}

// Capture real archive pages before composition so these boundary tests don't
// require repeated SQL reads to finish within one request deadline.
func snapshotActivityBurstReads(ctx context.Context, t *testing.T, src Sources, queries ...store.BurstQuery) Sources {
	t.Helper()
	snapshot := anchorBurstSnapshot{ActivitySource: src.Activity, restricted: sourcescope.Restricted(ctx), sourceIDs: slices.Clone(sourcescope.IDs(ctx))}
	for _, q := range queries {
		for {
			page, err := src.Activity.ListActivityBurstsContext(ctx, q)
			require.NoError(t, err)
			snapshot.reads = append(snapshot.reads, anchorBurstRead{query: q, page: page})
			if page.NextKey == nil {
				break
			}
			q.AfterKey = page.NextKey
		}
	}
	src.Activity = snapshot
	return src
}

// Capture each authorized probe while keeping display and policy reads live.
func snapshotAnchorBurstReads(ctx context.Context, t *testing.T, src Sources, req Request, anchorID int64) Sources {
	t.Helper()
	require.Empty(t, req.With)
	require.False(t, req.Conversation)
	display, err := src.Activity.MessageDisplayContext(ctx, []int64{anchorID})
	require.NoError(t, err)
	require.NotNil(t, display[anchorID].OccurredAt)
	at := display[anchorID].OccurredAt.In(src.Location)
	filters := src.Filters
	filters.IncludeNoise = req.IncludeNoise
	queries := []store.BurstQuery{}
	for _, days := range []int{1, 2, 4, 8, 16, 31} {
		from := at.AddDate(0, 0, -days)
		to := at.AddDate(0, 0, days).Add(time.Nanosecond)
		q := store.BurstQuery{After: &from, Before: &to, Filters: filters, Limit: 5000}
		queries = append(queries, q)
	}
	return snapshotActivityBurstReads(ctx, t, src, queries...)
}

func TestAnchorNeighboursUseRequestedMoment(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	anchor := f.add(f.now.Add(-3*time.Hour), false)
	f.add(f.now.Add(-time.Hour), false)
	for _, hour := range []int{4, 2} {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("neighbor-%d", hour), "Neighbor")
		requirements.NoError(err)
		f.add(f.now.Add(-time.Duration(hour)*time.Hour), false, func(m *store.Message) { m.ConversationID = conv })
	}
	f.project()
	req := anchorRequest(messageRef(anchor))
	req.Before, req.After = 1, 1
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 3)
	assertions.WithinDuration(f.now.Add(-4*time.Hour), got.History[0].Time, time.Microsecond)
	assertions.WithinDuration(f.now.Add(-2*time.Hour), got.History[1].Time, time.Microsecond)
	assertions.True(got.History[2].Anchor)
}

func TestAnchorNeighboursChronologicalWithProbeGrowth(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	var anchor int64
	for _, day := range []int{-20, -8, -3, 0, 4, 9, 22} {
		id := f.add(f.now.AddDate(0, 0, day), false)
		if day == 0 {
			anchor = id
		}
	}
	f.project()
	req := anchorRequest(messageRef(anchor))
	req.Before, req.After = 3, 3
	src := snapshotAnchorBurstReads(t.Context(), t, f.src, req, anchor)
	got, err := Build(t.Context(), src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 7)
	for i, e := range got.History {
		if i > 0 {
			assertions.True(e.Time.After(got.History[i-1].Time))
		}
	}
	assertions.True(got.History[3].Anchor)
}
func TestAnchorFilteredAnchorStillShown(t *testing.T) {
	for _, tc := range []struct {
		name           string
		messages       int
		list, filtered bool
	}{
		{"single list message", 1, true, true},
		{"mixed list burst", 2, true, false},
		{"oldest message in burst", 8, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineFixture(t)
			var id int64
			for i := range tc.messages {
				added := f.add(f.now.Add(time.Duration(i)*time.Second), false)
				if i == 0 {
					id = added
				}
			}
			f.project()
			if tc.list {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE messages SET list_id='list.example.com' WHERE id=?"), id)
				requirements.NoError(err)
			}
			req := anchorRequest(messageRef(id))
			src := snapshotAnchorBurstReads(t.Context(), t, f.src, req, id)
			got, err := Build(t.Context(), src, req)
			requirements.NoError(err)
			requirements.NotNil(got.Anchor)
			assertions.Equal(tc.filtered, got.Anchor.Filtered)
			requirements.Len(got.History, 1)
			assertions.Equal(tc.filtered, got.History[0].Filtered)
			assertions.True(got.History[0].Anchor)
			if !tc.filtered {
				assertions.Equal("burst", got.History[0].Kind)
			}
		})
	}
}
func TestAnchorUndatedRefused(t *testing.T) {
	f := newTimelineFixture(t)
	id := f.add(f.now, false, func(m *store.Message) {
		m.SentAt = sql.NullTime{}
		m.InternalDate = sql.NullTime{}
		m.ReceivedAt = sql.NullTime{}
	})
	_, err := Build(t.Context(), f.src, anchorRequest(messageRef(id)))
	assert.ErrorIs(t, err, ErrUndatedAnchor)
}
func TestAnchorDateOnlyStartsAtMidnight(t *testing.T) {
	f := newTimelineFixture(t)
	loc, err := time.LoadLocation("America/Vancouver")
	require.NoError(t, err)
	f.src.Location = loc
	got, err := Build(t.Context(), f.src, anchorRequest("2026-10-22"))
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 10, 22, 0, 0, 0, 0, loc), got.Anchor.Time)
}
func TestAnchorBareConversationUsesLatestEligible(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now.Add(-time.Hour), false)
	f.add(f.now, false)
	f.project()
	got, err := Build(t.Context(), f.src, anchorRequest(fmt.Sprintf("conv:%d", f.ConvID)))
	require.NoError(t, err)
	assert.WithinDuration(t, f.now, got.Anchor.Time, time.Microsecond)
}
func TestAnchorWithPersonNarrows(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineFixture(t)
	p := f.savePerson()
	id := f.add(f.now, false)
	other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	conv, err := f.Store.EnsureConversation(f.Source.ID, "other", "Other")
	requirements.NoError(err)
	otherID := f.add(f.now.Add(time.Hour), false, func(m *store.Message) { m.SenderID = sql.NullInt64{Int64: other, Valid: true}; m.ConversationID = conv })
	requirements.NoError(f.Store.ReplaceMessageRecipients(otherID, "from", []int64{other}, nil))
	f.project()
	req := anchorRequest(messageRef(id))
	req.With = fmt.Sprintf("person:%d", p)
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 1)
}
func TestAnchorEveryEntryAnchorRefRoundTrips(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now, false)
	f.transcript("unlinked", "", f.now.Add(-time.Hour))
	f.calendar("event", singleDayEvent)
	_, err := f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-10-22", Body: "Synthetic note", Author: "owner"})
	require.NoError(t, err)
	f.project()
	f.call(f.now.Add(-2 * time.Hour))
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(allDayEntries(got)), 5)
	for _, entry := range allDayEntries(got) {
		t.Run(entry.Kind, func(t *testing.T) {
			around, e := Build(t.Context(), f.src, anchorRequest(entry.AnchorRef))
			require.NoError(t, e)
			require.NotNil(t, around.Anchor)
			if entry.Kind != "note" {
				found := false
				for _, v := range around.History {
					found = found || v.Anchor
				}
				assert.True(t, found)
			}
		})
	}
}

func TestAnchorUsesDeduplicatedEmailRepresentative(t *testing.T) {
	f := newTimelineFixture(t)
	representative := f.add(f.now.Add(-time.Hour), false, func(m *store.Message) {
		m.RFC822MessageID = sql.NullString{String: "<shared@example.com>", Valid: true}
	})
	// A fuller conversation selects this account, even when the copy is the anchor.
	f.add(f.now.Add(-30*time.Minute), false)
	source, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
	require.NoError(t, err)
	require.NoError(t, f.Store.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	conv, err := f.Store.EnsureConversationWithType(source.ID, "copy", "email_thread", "Copy")
	require.NoError(t, err)
	copyID := f.add(f.now.Add(-time.Hour), false, func(m *store.Message) {
		m.SourceID, m.ConversationID = source.ID, conv
		m.RFC822MessageID = sql.NullString{String: "shared@example.com", Valid: true}
	})
	f.project()
	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(copyID)))
	require.NoError(t, err)
	require.NotNil(t, got.Anchor)
	assert.False(t, got.Anchor.Filtered)
	require.Len(t, got.History, 1)
	assert.True(t, got.History[0].Anchor)
	require.NotNil(t, got.History[0].Burst)
	assert.Contains(t, got.History[0].Burst.LatestMessageIDs, representative)
}

func TestSkippedMidnightKeepsTimelineCivilDate(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	require.NoError(t, err)
	ref, err := ParseAnchor("2026-09-06", loc)
	require.NoError(t, err)
	assert.Equal(t, "2026-09-06T01:00:00-03:00", ref.Time.Format(time.RFC3339))
	f := newTimelineFixture(t)
	f.src.Location = loc
	_, err = f.Store.CreateDailyNoteEntryContext(t.Context(), store.DailyNoteEntryInput{LocalDate: "2026-09-06", Body: "Civil date", Author: "owner"})
	require.NoError(t, err)
	id := f.calendar("skipped-midnight", `{"id":"skipped-midnight","summary":"All day","start":{"date":"2026-09-06"},"end":{"date":"2026-09-07"}}`)
	got, err := Build(t.Context(), f.src, anchorRequest("2026-09-06"))
	require.NoError(t, err)
	assert.Equal(t, "2026-09-06", got.Anchor.Time.In(loc).Format(time.DateOnly))
	require.Len(t, got.History, 2)
	for _, entry := range got.History {
		assert.Equal(t, "2026-09-06", entry.Time.In(loc).Format(time.DateOnly))
	}
	meeting, err := Build(t.Context(), f.src, anchorRequest(fmt.Sprintf("evt:%d", id)))
	require.NoError(t, err)
	assert.Equal(t, "2026-09-06", meeting.Anchor.Time.In(loc).Format(time.DateOnly))
}
