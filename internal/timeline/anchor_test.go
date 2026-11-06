package timeline

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"strings"
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
	requirements.NotNil(got.Anchor)
	assertions.Equal(messageRef(anchor), got.Anchor.Ref)
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
	for i, offset := range []int{-20, -8, -3, 0, 4, 9, 22} {
		assertions.WithinDuration(f.now.AddDate(0, 0, offset), got.History[i].Time, time.Microsecond)
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
	for _, with := range []string{fmt.Sprintf("person:%d", p), "email:alice@example.com"} {
		req := anchorRequest(messageRef(id))
		req.With = with
		got, err := Build(t.Context(), f.src, req)
		requirements.NoError(err)
		requirements.Len(got.History, 1)
	}
}
func TestAnchorEveryEntryAnchorRefRoundTrips(t *testing.T) {
	f := newTimelineFixture(t)
	f.add(f.now, false)
	f.transcript("unlinked", "", f.now.Add(-time.Hour))
	f.calendar("event", singleDayEvent)
	f.project()
	f.call(f.now.Add(-2 * time.Hour))
	got, err := Build(t.Context(), f.src, dayRequest())
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(allDayEntries(got)), 4)
	for _, entry := range allDayEntries(got) {
		t.Run(entry.Kind, func(t *testing.T) {
			around, e := Build(t.Context(), f.src, anchorRequest(entry.AnchorRef))
			require.NoError(t, e)
			require.NotNil(t, around.Anchor)
			found := false
			for _, v := range around.History {
				found = found || v.Anchor
			}
			assert.True(t, found)
		})
	}
}

func TestSkippedMidnightKeepsTimelineCivilDate(t *testing.T) {
	loc, err := time.LoadLocation("America/Santiago")
	require := require.New(t)
	require.NoError(err)
	assert := assert.New(t)
	f := newTimelineFixture(t)
	f.src.Location = loc
	id := f.calendar("skipped-midnight", `{"id":"skipped-midnight","summary":"All day","start":{"date":"2026-09-06"},"end":{"date":"2026-09-07"}}`)
	got, err := Build(t.Context(), f.src, anchorRequest("2026-09-06"))
	require.NoError(err)
	assert.Equal("2026-09-06", got.Anchor.Time.In(loc).Format(time.DateOnly))
	require.Len(got.History, 1)
	for _, entry := range got.History {
		assert.Equal("2026-09-06", entry.Time.In(loc).Format(time.DateOnly))
	}
	meeting, err := Build(t.Context(), f.src, anchorRequest(fmt.Sprintf("evt:%d", id)))
	require.NoError(err)
	assert.Equal("2026-09-06", meeting.Anchor.Time.In(loc).Format(time.DateOnly))
}

func TestTimelineAnchorWithBccSelectsMatchingCopy(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	bcc := f.EnsureParticipant("bcc@example.com", "Bcc contact", "example.com")
	f.add(f.now, false, func(m *store.Message) {
		m.RFC822MessageID = sql.NullString{String: "<bcc-copy@example.com>", Valid: true}
	})
	source, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
	conv, err := f.Store.EnsureConversationWithType(source.ID, "bcc-copy", "email_thread", "Bcc copy")
	requirements.NoError(err)
	copyID := f.add(f.now, false, func(m *store.Message) {
		m.SourceID, m.ConversationID = source.ID, conv
		m.RFC822MessageID = sql.NullString{String: "<bcc-copy@example.com>", Valid: true}
	})
	requirements.NoError(f.Store.ReplaceMessageRecipients(copyID, "bcc", []int64{bcc}, nil))
	f.project()
	req := anchorRequest(messageRef(copyID))
	req.With = "email:bcc@example.com"
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 1)
	assertions.Equal(messageRef(copyID), got.History[0].AnchorRef)
	assertions.True(got.History[0].Anchor)
}

func TestTimelineDefaultWindowAfterSkippedMidnight(t *testing.T) {
	f := newTimelineFixture(t)
	loc, err := time.LoadLocation("America/Santiago")
	require.NoError(t, err)
	f.src.Location = loc
	f.src.Clock = func() time.Time { return time.Date(2026, 9, 7, 0, 30, 0, 0, loc) }
	req := dayRequest()
	req.Date = ""
	got, err := Build(t.Context(), f.src, req)
	require.NoError(t, err)
	assert.Equal(t, Window{From: "2026-09-06", To: "2026-09-07"}, got.Window)
}

func TestColdConversationIncludesAllMessages(t *testing.T) {
	for _, cold := range []bool{false, true} {
		t.Run(fmt.Sprintf("cold=%t", cold), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newTimelineFixture(t)
			conv, err := f.Store.EnsureConversation(f.Source.ID, "cold-conversation", "Cold conversation")
			requirements.NoError(err)
			if cold {
				requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, "cold-conversation"))
			}
			ids := []int64{}
			n := 30
			if cold {
				n = 8
			}
			for i := range n {
				at := f.now.Add(-time.Hour + time.Duration(i)*time.Second)
				if cold {
					at = f.now.Add(-time.Hour + time.Duration(i/2)*time.Minute)
				}
				ids = append(ids, f.add(at, !cold && i%2 == 0, func(m *store.Message) {
					m.ConversationID = conv
					m.Snippet = sql.NullString{String: strings.Repeat("message ", 30), Valid: true}
				}))
			}
			f.project()
			before := n / 2
			if cold {
				before = 2
			}
			req := Request{Around: messageRef(ids[before]), Conversation: true, Before: before, After: n - before - 1, Scope: Scope{Owner: true}, Format: "json", Budget: 65536}
			if cold {
				hiddenReq := req
				hiddenReq.Before, hiddenReq.After = 0, 0
				hiddenReq.BeforeSet, hiddenReq.AfterSet = true, true
				hidden, err := Build(t.Context(), f.src, hiddenReq)
				requirements.NoError(err)
				requirements.Len(hidden.History, 1)
				assertions.True(hidden.Anchor.Filtered)
				assertions.Equal(8, hidden.Omitted.Cold)
			}
			req.IncludeCold = cold
			got, err := Build(t.Context(), f.src, req)
			requirements.NoError(err)
			requirements.Len(got.History, n)
			assertions.False(got.Anchor.Filtered)
			assertions.Zero(got.Omitted.Cold)
			want := []string{}
			for _, id := range ids {
				want = append(want, messageRef(id))
			}
			anchor := anchorRequest(messageRef(ids[len(ids)-1]))
			anchor.IncludeCold = cold
			message, err := Build(t.Context(), f.src, anchor)
			requirements.NoError(err)
			t.Run("conversation anchor", func(t *testing.T) {
				anchor.Around = fmt.Sprintf("conv:%d", conv)
				conversation, err := Build(t.Context(), f.src, anchor)
				require.NoError(t, err)
				assert.Equal(t, message.Anchor.Time, conversation.Anchor.Time)
				latest := f.now.Add(-time.Hour + time.Duration(n-1)*time.Second)
				if cold {
					latest = f.now.Add(-time.Hour + time.Duration((n-1)/2)*time.Minute)
				}
				assert.WithinDuration(t, latest, conversation.Anchor.Time, time.Microsecond)
				assert.Equal(t, message.History, conversation.History)
			})

			req.Budget = 4096
			seen := map[string]bool{}
			refs := []string{}
			pages := 0
			for page := range 20 {
				got, err = Build(t.Context(), f.src, req)
				requirements.NoError(err)
				requirements.NotEmpty(got.History)
				pages++
				for _, entry := range got.History {
					assertions.False(seen[entry.Ref], "duplicate entry on page %d", page)
					seen[entry.Ref] = true
					refs = append(refs, entry.Ref)
					assertions.Equal("message", entry.Kind)
					assertions.True(strings.HasPrefix(entry.Key, "msg:"))
				}
				if got.Cursor == "" {
					break
				}
				req.Cursor = got.Cursor
			}
			assertions.Greater(pages, 1)
			assertions.Equal(want, refs)
		})
	}
}
