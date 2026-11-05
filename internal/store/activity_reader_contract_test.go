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
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	f.peer = f.EnsureParticipant("noreply@example.com", "Peer", "example.com")
	alias := f.EnsureParticipant("noreply@other.example.com", "Alias", "other.example.com")
	_, err := f.Store.LinkParticipants(f.peer, alias)
	require.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	require.NoError(err)
	for i := range 3 {
		f.add(t, f.ConvID, f.Source.ID, tierAsOf.AddDate(0, 0, i-3), i == 1, "")
	}
	hidden, err := f.Store.GetOrCreateSource("matrix", "hidden@example.com")
	require.NoError(err)
	conv, err := f.Store.EnsureConversation(hidden.ID, "hidden", "Hidden")
	require.NoError(err)
	f.peer = alias
	f.add(t, conv, hidden.ID, tierAsOf.AddDate(0, 0, -1), false, "")
	projectTimelineActivity(t, f, "UTC")
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
	before, err := f.Store.ExplainPersonTierContext(ctx, person.ID, tierOptions())
	require.NoError(err)
	require.Equal(3, before.Tier)
	require.NoError(f.Store.SetParticipantIdentifier(alias, "matrix", "@alias:example.com"))
	after, err := f.Store.ExplainPersonTierContext(ctx, person.ID, tierOptions())
	require.NoError(err)
	assert.Equal(before.Tier, after.Tier)
	assert.Equal(before.Signals, after.Signals)
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
			var personID int64
			if saved {
				person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				require.NoError(err)
				personID = person.ID
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
			if saved {
				tier, err := f.Store.ExplainPersonTierContext(t.Context(), personID, tierOptions())
				require.NoError(err)
				require.NotNil(tier.Exclusion)
				assert.Equal("owner", *tier.Exclusion)
			}
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
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	day := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.add(t, f.ConvID, f.Source.ID, day.AddDate(0, 0, -1), false, "")
	f.add(t, f.ConvID, f.Source.ID, day, false, "")
	projectTimelineActivity(t, f, "UTC")
	before := day.Add(12 * time.Hour)
	page, err := f.Store.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Before: &before, Limit: 10})
	require.NoError(err)
	require.Len(page.Bursts, 1)
	assert.Equal("2026-10-22", page.Bursts[0].LocalDate)
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
