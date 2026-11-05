package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func TestScopedTierHiddenIdentifier(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	f.peer = f.EnsureParticipant("noreply@example.com", "Peer", "example.com")
	alias := f.EnsureParticipant("noreply@other.example.com", "Alias", "other.example.com")
	_, err := f.Store.LinkParticipants(f.peer, alias)
	requirements.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	q := timelineContactQuery(f)
	q.Now = tierAsOf
	for i := range 3 {
		f.add(t, f.ConvID, f.Source.ID, tierAsOf.AddDate(0, 0, i-3), i == 1, "")
	}
	hidden, err := f.Store.GetOrCreateSource("matrix", "hidden@example.com")
	requirements.NoError(err)
	conv, err := f.Store.EnsureConversation(hidden.ID, "hidden", "Hidden")
	requirements.NoError(err)
	peer := f.peer
	f.peer = alias
	hiddenAt := q.Now.AddDate(0, 0, -1)
	f.add(t, conv, hidden.ID, hiddenAt, false, "")
	f.peer = peer
	projectTimelineActivity(t, f, "UTC")
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	before, err := f.Store.ExplainPersonTierContext(ctx, person.ID, tierOptions())
	requirements.NoError(err)
	requirements.Equal(3, before.Tier)
	requirements.NoError(f.Store.SetParticipantIdentifier(alias, "matrix", "@alias:example.com"))
	after, err := f.Store.ExplainPersonTierContext(ctx, person.ID, tierOptions())
	requirements.NoError(err)
	assertions.Equal(before.Tier, after.Tier)
	assertions.Equal(before.Signals, after.Signals)
}

func TestScopedActivityHiddenIdentifier(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	f.peer = f.EnsureParticipant("noreply@example.com", "Peer", "example.com")
	alias := f.EnsureParticipant("noreply@other.example.com", "Alias", "other.example.com")
	_, err := f.Store.LinkParticipants(f.peer, alias)
	requirements.NoError(err)
	_, _, err = f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	q := timelineContactQuery(f)
	f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
	hidden, err := f.Store.GetOrCreateSource("matrix", "hidden@example.com")
	requirements.NoError(err)
	conv, err := f.Store.EnsureConversation(hidden.ID, "hidden", "Hidden")
	requirements.NoError(err)
	peer := f.peer
	f.peer = alias
	hiddenAt := q.Now
	f.add(t, conv, hidden.ID, hiddenAt, false, "")
	f.peer = peer
	projectTimelineActivity(t, f, "UTC")
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})

	before, err := f.Store.MessageContactAggregateContext(ctx, q)
	requirements.NoError(err)
	requirements.Zero(before.Count)
	requirements.NoError(f.Store.SetParticipantIdentifier(alias, "matrix", "@alias:example.com"))
	after, err := f.Store.MessageContactAggregateContext(ctx, q)
	requirements.NoError(err)
	assertions.Equal(before, after)
	bq := timelineBurstQuery()
	bq.Filters.IncludeNoise = true
	bursts, err := f.Store.ListActivityBurstsContext(ctx, bq)
	requirements.NoError(err)
	requirements.Len(bursts.Bursts, 1)
	assertions.Zero(bursts.Bursts[0].Inbound)
	for _, cp := range bursts.Bursts[0].Counterparts {
		if cp.ParticipantID == f.peer {
			assertions.Equal("automated_address", cp.Excluded)
			assertions.False(cp.Counted)
		}
	}
}

func TestTierAndTimelineOwnerAgree(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name := "unsaved"
		if saved {
			name = "saved"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newTimelineActivityFixture(t)
			subject := store.ActivitySubject{ParticipantIDs: []int64{f.peer}}
			if saved {
				person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				require.NoError(err)
				subject = store.ActivitySubject{PersonID: person.ID}
			}
			other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
			require.NoError(err)
			require.NoError(f.Store.AddAccountIdentity(other.ID, "alice@example.com", "manual"))
			f.add(t, f.ConvID, f.Source.ID, tierAsOf, false, "")
			projectTimelineActivity(t, f, "UTC")
			q := timelineContactQuery(f)
			q.Subject = subject
			q.Now = tierAsOf
			got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			require.NoError(err)
			assert.Zero(got.Count)
			bq := timelineBurstQuery()
			bq.Filters.IncludeNoise = true
			page, err := f.Store.ListActivityBurstsContext(t.Context(), bq)
			require.NoError(err)
			require.Len(page.Bursts, 1)
			for _, cp := range page.Bursts[0].Counterparts {
				if cp.ParticipantID == f.peer {
					assert.Equal("owner", cp.Excluded)
					assert.False(cp.Counted)
				}
			}
			ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
			got, err = f.Store.MessageContactAggregateContext(ctx, q)
			require.NoError(err)
			assert.Equal(1, got.Count, "owner identities outside the grant do not exclude contact")
		})
	}
}

func TestCoauthorExclusionsPreserveHumanCredit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	require.NoError(err)
	automated := f.EnsureParticipant("noreply@example.com", "Automated", "example.com")
	id := f.add(t, f.ConvID, f.Source.ID, tierAsOf, false, "")
	require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{f.peer, automated}, nil))
	projectTimelineActivity(t, f, "UTC")
	tier, err := f.Store.ExplainPersonTierContext(t.Context(), person.ID, tierOptions())
	require.NoError(err)
	require.Equal(1, tier.Signals.InboundLong)
	q := timelineContactQuery(f)
	q.Now = tierAsOf
	q.Subject = store.ActivitySubject{PersonID: person.ID}
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(err)
	assert.Equal(1, got.Count)
	page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(err)
	require.Len(page.Bursts, 1)
	assert.Equal(1, page.Bursts[0].Inbound)
}

func TestCopyWinnerIndependentOfWindow(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineActivityFixture(t)
	day := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	shared := f.add(t, f.ConvID, f.Source.ID, day, false, "<shared@example.com>")
	f.add(t, f.ConvID, f.Source.ID, day.AddDate(0, 0, 1), true, "<reply1@example.com>")
	f.add(t, f.ConvID, f.Source.ID, day.AddDate(0, 0, 1).Add(time.Hour), false, "<reply2@example.com>")
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(other.ID, "owner@example.com", "manual"))
	conv, err := f.Store.EnsureConversation(other.ID, "other-thread", "Other")
	requirements.NoError(err)
	f.add(t, conv, other.ID, day, false, "<shared@example.com>")
	f.add(t, conv, other.ID, day.Add(time.Hour), true, "<other@example.com>")
	projectTimelineActivity(t, f, "UTC")
	for _, to := range []string{"2026-10-20", "2026-10-21"} {
		t.Run(to, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			page, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: "2026-10-20", ToDate: to, Limit: 10})
			require.NoError(err)
			found := false
			for _, b := range page.Bursts {
				if b.ConversationID == f.ConvID && b.LocalDate == "2026-10-20" {
					found = true
					assert.Equal([]int64{shared}, b.LatestMessageIDs)
					assert.Equal(1, b.Inbound)
					assert.Equal(2, b.CopyCounts[shared])
				}
			}
			assert.True(found, "the full three-message thread wins in every window")
		})
	}
}

func TestPersonReferenceRejectsMalformedEmail(t *testing.T) {
	f := newTimelineActivityFixture(t)
	for _, value := range []string{"", "alice", "alice@", "@example.com", "Alice <alice@example.com>", "alice@example.com,other@example.com"} {
		t.Run(value, func(t *testing.T) {
			_, err := f.Store.ResolvePersonReferenceContext(t.Context(), "email", value, nil, 10)
			assert.ErrorIs(t, err, store.ErrInvalidActivityRequest)
		})
	}
}

func TestActivityWindowDateAndTimestampBounds(t *testing.T) {
	f := newTimelineActivityFixture(t)
	day := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.add(t, f.ConvID, f.Source.ID, day.AddDate(0, 0, -1), false, "")
	f.add(t, f.ConvID, f.Source.ID, day, false, "")
	projectTimelineActivity(t, f, "UTC")
	before := day.Add(-time.Hour)
	for _, tc := range []struct {
		name, from, to, want string
		before               *time.Time
	}{
		{"date range", "2026-10-22", "2026-10-22", "2026-10-22", nil},
		{"timestamp cuts admitted date", "2026-10-21", "2026-10-22", "2026-10-21", &before},
		{"both bounds exclude", "2026-10-22", "2026-10-22", "", &before},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			page, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: tc.from, ToDate: tc.to, Before: tc.before, Limit: 10})
			require.NoError(err)
			if tc.want == "" {
				assert.Empty(page.Bursts)
			} else {
				require.Len(page.Bursts, 1)
				assert.Equal(tc.want, page.Bursts[0].LocalDate)
			}
		})
	}
}

func TestActivityWindowValidatesDatesWithTimestamps(t *testing.T) {
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	before := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	for _, dates := range [][2]string{{"bad", "2026-10-22"}, {"2026-10-22", ""}, {"", "2026-10-22"}, {"2026-10-23", "2026-10-22"}} {
		_, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: dates[0], ToDate: dates[1], Before: &before, Limit: 10})
		assert.ErrorIs(err, store.ErrInvalidActivityRequest)
	}
}

func TestActivityInstantWindowReturnsCompleteConversationDays(t *testing.T) {
	requirements := require.New(t)
	f := newTimelineActivityFixture(t)
	// This local day crosses UTC midnight. Adjacent days and conversations
	// must stay out even when their messages share the selected local date.
	first := time.Date(2026, 10, 22, 13, 0, 0, 0, time.UTC)
	morning := f.add(t, f.ConvID, f.Source.ID, first, false, "")
	afternoon := f.add(t, f.ConvID, f.Source.ID, first.Add(6*time.Hour), false, "")
	last := first.Add(12 * time.Hour)
	reply := f.add(t, f.ConvID, f.Source.ID, last, true, "")
	f.add(t, f.ConvID, f.Source.ID, first.AddDate(0, 0, -1), false, "")
	f.add(t, f.ConvID, f.Source.ID, first.AddDate(0, 0, 1), false, "")
	other, err := f.Store.EnsureConversation(f.Source.ID, "outside-window", "Other")
	requirements.NoError(err)
	f.add(t, other, f.Source.ID, first, false, "")
	noise := f.add(t, f.ConvID, f.Source.ID, first.Add(time.Hour), false, "")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='list.example.com' WHERE id=?`), noise)
	requirements.NoError(err)
	projectTimelineActivity(t, f, "America/New_York")
	for _, tc := range []struct {
		name          string
		after, before *time.Time
	}{
		{"after", new(first.Add(3 * time.Hour)), new(last.Add(time.Hour))},
		{"before", new(first.Add(-time.Hour)), new(first.Add(3 * time.Hour))},
		{"middle", new(first.Add(3 * time.Hour)), new(first.Add(7 * time.Hour))},
		{"noise-only instant", new(first.Add(time.Hour)), new(first.Add(2 * time.Hour))},
		{"inclusive after", new(last), new(last.Add(time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			q := store.BurstQuery{After: tc.after, Before: tc.before, Limit: 10}
			if tc.name == "before" {
				q.ConversationID = f.ConvID
			}
			page, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(page.Bursts, 1)
			b := page.Bursts[0]
			assertions.Equal("2026-10-22", b.LocalDate)
			assertions.Equal(2, b.Inbound)
			assertions.Equal(1, b.Outbound)
			assertions.Equal(first, b.FirstAt.UTC())
			assertions.Equal(last, b.LastAt.UTC())
			assertions.Equal(reply, b.AnchorMessageID)
			assertions.Equal([]int64{reply, afternoon, morning}, b.LatestMessageIDs)
			assertions.Equal(1, page.NoiseOmitted)
			messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, After: tc.after, Before: tc.before, Limit: 10})
			requirements.NoError(err)
			ids := make([]int64, len(messages))
			for i, m := range messages {
				ids[i] = m.MessageID
			}
			assertions.Equal([]int64{morning, afternoon, reply}, ids)
		})
	}
	t.Run("empty interval within day", func(t *testing.T) {
		requirements, assertions := require.New(t), assert.New(t)
		after, before := first.Add(3*time.Hour), first.Add(6*time.Hour)
		page, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{After: &after, Before: &before, Limit: 10})
		requirements.NoError(err)
		assertions.Empty(page.Bursts)
		assertions.Zero(page.NoiseOmitted)
		messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, After: &after, Before: &before, Limit: 10})
		requirements.NoError(err)
		assertions.Empty(messages)
	})
}

func TestActivityInstantWindowSingleBounds(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	f := newTimelineActivityFixture(t)
	first := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	morning := f.add(t, f.ConvID, f.Source.ID, first, false, "")
	afternoon := f.add(t, f.ConvID, f.Source.ID, first.Add(6*time.Hour), false, "")
	projectTimelineActivity(t, f, "UTC")
	noon := first.Add(3 * time.Hour)
	for _, q := range []store.BurstQuery{{After: &noon, Limit: 10}, {Before: &noon, Limit: 10}} {
		page, err := f.Store.ListActivityBurstsContext(t.Context(), q)
		requirements.NoError(err)
		requirements.Len(page.Bursts, 1)
		assertions.Equal(2, page.Bursts[0].Inbound)
		assertions.Equal(first, page.Bursts[0].FirstAt.UTC())
		assertions.Equal(afternoon, page.Bursts[0].AnchorMessageID)
		messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, After: q.After, Before: q.Before, Limit: 10})
		requirements.NoError(err)
		requirements.Len(messages, 2)
		assertions.Equal(morning, messages[0].MessageID)
		assertions.Equal(afternoon, messages[1].MessageID)
	}
	// A deleted message still has projected activity, but cannot select a day.
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`), afternoon)
	requirements.NoError(err)
	page, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{After: &noon, Limit: 10})
	requirements.NoError(err)
	assertions.Empty(page.Bursts)
	messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, After: &noon, Limit: 10})
	requirements.NoError(err)
	assertions.Empty(messages)
}
