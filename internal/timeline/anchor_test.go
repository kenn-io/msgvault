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

	reads      []anchorBurstRead
	restricted bool
	sourceIDs  []int64
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

// These tests exercise repeated anchor probes and mutation visibility, rather
// than requiring six real SQL reads to finish within one request deadline.
// Capture the actual authorized reader for each Build; delegate every other
// method so display, latest-anchor and projection-state reads remain live.
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
	snapshot := anchorBurstSnapshot{ActivitySource: src.Activity, restricted: sourcescope.Restricted(ctx), sourceIDs: slices.Clone(sourcescope.IDs(ctx))}
	for _, days := range []int{1, 2, 4, 8, 16, 31} {
		from := at.AddDate(0, 0, -days)
		to := at.AddDate(0, 0, days).Add(time.Nanosecond)
		q := store.BurstQuery{After: &from, Before: &to, Filters: filters, Limit: 5000}
		page, err := src.Activity.ListActivityBurstsContext(ctx, q)
		require.NoError(t, err)
		snapshot.reads = append(snapshot.reads, anchorBurstRead{query: q, page: page})
	}
	src.Activity = snapshot
	return src
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

func TestOwnerAnchorCanFollowSourceDeletedMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	id := f.add(f.now, false)
	person := f.savePerson()
	f.project()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), f.now, id)
	requirements.NoError(err)

	req := anchorRequest(messageRef(id))
	req.Mode = ModeAnchor
	req.With = fmt.Sprintf("person:%d", person)
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.Equal(messageRef(id), got.Anchor.Ref)
	var anchor *Entry
	for i := range got.History {
		if got.History[i].Anchor {
			anchor = &got.History[i]
			break
		}
	}
	requirements.NotNil(anchor)
	requirements.NotNil(anchor.Message)
	assertions.Equal("Synthetic message", anchor.Message.Snippet.Text)

	_, err = Build(sourcescope.WithSourceIDs(t.Context(), []int64{}), f.src, req)
	requirements.ErrorIs(err, ErrUnknownReference)

	localID := f.add(f.now.Add(time.Minute), false)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=? WHERE id=?`), f.now, localID)
	requirements.NoError(err)
	_, err = Build(t.Context(), f.src, anchorRequest(messageRef(localID)))
	requirements.ErrorIs(err, ErrUnknownReference)
}
func TestAnchorFilteredAnchorStillShown(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	id := f.add(f.now, false)
	f.project()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE messages SET list_id='list.example.com' WHERE id=?"), id)
	requirements.NoError(err)
	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(id)))
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.True(got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assertions.True(got.History[0].Filtered)
	assertions.True(got.History[0].Anchor)
}
func TestCalendarAnchorSurvivesPersonAndTierFilters(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*timelineFixture, *Request)
	}{
		{
			name: "person",
			set: func(f *timelineFixture, req *Request) {
				f.EnsureParticipant("bob@example.com", "Bob", "example.com")
				req.With = "email:bob@example.com"
			},
		},
		{
			name: "tier",
			set: func(f *timelineFixture, req *Request) {
				person := f.savePerson()
				f.tier(person, 1)
				req.Tiers = []int{2}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newTimelineFixture(t)
			event := f.calendar("filtered-anchor", singleDayEvent)
			req := anchorRequest(fmt.Sprintf("evt:%d", event))
			tc.set(f, &req)
			f.project()

			got, err := Build(t.Context(), f.src, req)
			requirements.NoError(err)
			requirements.NotNil(got.Anchor)
			assertions.True(got.Anchor.Filtered)
			requirements.Len(got.History, 1)
			entry := got.History[0]
			assertions.Equal(entryKindMeeting, entry.Kind)
			assertions.Equal(fmt.Sprintf("evt:%d", event), entry.Ref)
			assertions.True(entry.Anchor)
			assertions.True(entry.Filtered)
			assertions.Empty(entry.Meeting.Attendees)
			assertions.Empty(entry.Meeting.People)
			requirements.NoError(entry.Validate())
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
func TestDateOnlyAnchorUsesCivilDateBoundary(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	loc, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	anchor, err := ParseAnchor("2018-11-04", loc)
	requirements.NoError(err)
	assertions.Equal(time.Date(2018, 11, 4, 1, 0, 0, 0, loc), anchor.Time)

	apia, err := time.LoadLocation("Pacific/Apia")
	requirements.NoError(err)
	_, err = ParseAnchor("2011-12-30", apia)
	assertions.ErrorIs(err, ErrUnknownReference)
}
func TestAllDayAnchorUsesCivilDateBoundary(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	loc, err := time.LoadLocation("America/Sao_Paulo")
	requirements.NoError(err)
	f := newTimelineFixture(t)
	f.src.Location = loc
	f.now = time.Date(2018, 11, 5, 12, 0, 0, 0, loc)
	f.src.Clock = func() time.Time { return f.now }
	event := f.calendar("skipped-midnight", "{\"id\":\"holiday\",\"iCalUID\":\"holiday@example.com\",\"summary\":\"Holiday\",\"start\":{\"date\":\"2018-11-04\"},\"end\":{\"date\":\"2018-11-05\"}}")
	f.project()

	got, err := Build(t.Context(), f.src, anchorRequest(fmt.Sprintf("evt:%d", event)))
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.Equal(time.Date(2018, 11, 4, 1, 0, 0, 0, loc), got.Anchor.Time)
	assertions.Equal("2018-11-04", got.Anchor.Time.In(loc).Format(time.DateOnly))
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

func TestAnchorBareColdConversationUsesLatestMessageWhenIncluded(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	conversation, err := f.Store.EnsureConversation(f.Source.ID, "cold-anchor", "Synthetic conversation")
	requirements.NoError(err)
	id := f.add(f.now.Add(-time.Hour), false, func(message *store.Message) {
		message.ConversationID = conversation
	})
	f.project()
	requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, "cold-anchor"))

	req := anchorRequest(fmt.Sprintf("conv:%d", conversation))
	req.Conversation, req.IncludeCold = true, true
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	requirements.Len(got.History, 1)
	assertions.True(got.History[0].Anchor)
	assertions.Equal(messageRef(id), got.History[0].Ref)
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
	bp, be := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Limit: 5000})
	t.Log("bursts", bp, be)
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

func TestAnchorOldestMessageInBurstIsNotFiltered(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	ids := []int64{}
	for i := range 8 {
		ids = append(ids, f.add(f.now.Add(time.Duration(i)*time.Second), false))
	}
	f.project()
	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(ids[0])))
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.False(got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assertions.Equal("burst", got.History[0].Kind)
	assertions.True(got.History[0].Anchor)
	assertions.False(got.History[0].Filtered)
}

func TestAnchorMissingSourcesProduceUnavailableGaps(t *testing.T) {
	for _, ref := range []string{"2026-10-22", "msg:1"} {
		t.Run(ref, func(t *testing.T) {
			f := newTimelineFixture(t)
			f.src.Activity = nil
			f.src.Calls = nil
			f.src.Calendar = nil
			f.src.Notes = nil
			got, err := Build(t.Context(), f.src, anchorRequest(ref))
			require.NoError(t, err)
			for _, section := range []string{"history", "calls", "meetings", "notes"} {
				assert.Contains(t, got.Gaps, Gap{Section: section, Code: "unavailable"})
			}
			require.NotEmpty(t, got.Freshness.Sources)
		})
	}
}

func TestAnchorMixedListBurstStaysAdmitted(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	id := f.add(f.now, false)
	f.add(f.now.Add(time.Second), false)
	f.project()
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind("UPDATE messages SET list_id='list.example.com' WHERE id=?"), id)
	requirements.NoError(err)
	got, err := Build(t.Context(), f.src, anchorRequest(messageRef(id)))
	requirements.NoError(err)
	requirements.NotNil(got.Anchor)
	assertions.False(got.Anchor.Filtered)
	requirements.Len(got.History, 1)
	assertions.Equal("burst", got.History[0].Kind)
	assertions.True(got.History[0].Anchor)
	assertions.False(got.History[0].Filtered)
}
