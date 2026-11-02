package timeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// The cap selects a prefix of the readers' shared (FirstAt, Key) ordering,
// before the day renderer applies its separate tier/exchange ranking.
func TestMergeBurstsSelectsCombinedReaderOrder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	var want []string
	for i, cold := range []bool{true, false, true, false} {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("ordered-%d", i), "Ordered")
		requirements.NoError(err)
		at := f.now.Add(time.Duration(i/2) * time.Minute)
		f.add(at, false, func(m *store.Message) { m.ConversationID = conv })
		if cold {
			requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, fmt.Sprintf("ordered-%d", i)))
		}
		want = append(want, fmt.Sprintf("burst:%d:%d:2026-10-22", f.Source.ID, conv))
	}
	f.project()
	q := store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Filters: f.src.Filters, Limit: 5000}
	warm, err := f.src.Activity.ListActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	cold, err := f.src.Cold.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(warm.Bursts, 2)
	requirements.Len(cold.Bursts, 2)
	// Equal-time ties use lexical keys, rather than source preference.
	slices.Sort(want[:2])
	slices.Sort(want[2:])
	merged := mergeBursts(warm.Bursts, cold.Bursts)
	got := []string{}
	for _, b := range merged[:3] {
		got = append(got, b.Key)
	}
	assertions.Equal(want[:3], got)
	// The deduplication path must retain the same selection order.
	merged = mergeBursts(warm.Bursts, append(cold.Bursts, warm.Bursts...))
	requirements.Len(merged, 4)
	assertions.Equal(want[0], merged[0].Key)
}

type countedTierPeople struct {
	PeopleSource

	calls [][]int64
	asOf  []time.Time
	fail  error
}

func (s *countedTierPeople) PersonTiersContext(ctx context.Context, ids []int64, options store.PersonTierOptions) (map[int64]store.PersonTier, error) {
	s.calls = append(s.calls, slices.Clone(ids))
	s.asOf = append(s.asOf, options.AsOf)
	tiers, err := s.PeopleSource.PersonTiersContext(ctx, ids, options)
	if err != nil {
		return nil, err
	}
	if s.fail != nil {
		return nil, s.fail
	}
	return tiers, nil
}

func TestBuildBatchesKnownPersonTiers(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	participants := []int64{f.peer}
	persons := []int64{f.savePerson()}
	for i := 1; i < 4; i++ {
		participant := f.EnsureParticipant(fmt.Sprintf("contact%d@example.com", i), fmt.Sprintf("Contact %d", i), "example.com")
		participants = append(participants, participant)
		if i < 3 {
			person, _, err := f.Store.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			persons = append(persons, person.ID)
		}
	}
	f.tier(persons[0], 1)
	f.tier(persons[1], 2)
	for i := range 24 {
		participant := participants[i%4]
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("tiers-%d", i), "Tier batching")
		requirements.NoError(err)
		id := f.add(f.now.Add(-time.Duration(i)*time.Minute), false, func(m *store.Message) {
			m.ConversationID = conv
			m.SenderID = sql.NullInt64{Int64: participant, Valid: true}
		})
		requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{participant}, nil))
	}
	f.project()
	counted := &countedTierPeople{PeopleSource: f.src.People}
	f.src.People = counted
	req := dayRequest()
	req.Tiers = []int{1}
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 6)
	for _, entry := range entries {
		requirements.Len(entry.Burst.People, 1)
		assertions.Equal(persons[0], entry.Burst.People[0].PersonID)
		assertions.Equal(1, entry.Burst.People[0].Tier)
		assertions.Equal("observed", entry.Burst.People[0].TierSource)
	}
	requirements.Len(counted.calls, 1, "tier source calls must not grow with the number of entries")
	assertions.ElementsMatch(persons, counted.calls[0], "known IDs are distinct, including missing tiers but excluding unsaved people")
	assertions.Equal([]time.Time{f.now}, counted.asOf, "tier rules use the pinned request clock")
	// The cache belongs to one Build, so a later tier update must be observed.
	f.tier(persons[0], 2)
	f.project()
	counted.calls = nil
	got, err = Build(t.Context(), f.src, req)
	requirements.NoError(err)
	assertions.Empty(allDayEntries(got))
	requirements.Len(counted.calls, 1)
}

func TestBuildTierFailureRemainsGapWithoutPerEntryRetries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	person := f.savePerson()
	f.tier(person, 1)
	for i := range 8 {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("tier-error-%d", i), "Tier failure")
		requirements.NoError(err)
		f.add(f.now.Add(-time.Duration(i)*time.Minute), false, func(m *store.Message) { m.ConversationID = conv })
	}
	f.project()
	counted := &countedTierPeople{PeopleSource: f.src.People, fail: errors.New("tier source unavailable")}
	f.src.People = counted
	req := dayRequest()
	req.Tiers = []int{1}
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	assertions.Empty(allDayEntries(got))
	assertions.Contains(got.Gaps, Gap{Section: "people", Code: "unavailable"})
	requirements.Len(counted.calls, 1)
	assertions.Equal([]int64{person}, counted.calls[0])
}

// Shared flags change history admission and message omissions, while the
// relationship header continues to describe default-filter contact evidence.
func TestSharedColdNoiseAcrossTimelineModes(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", saved), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := newTimelineFixture(t)
			ref := "email:alice@example.com"
			if saved {
				ref = fmt.Sprintf("person:%d", f.savePerson())
			}
			f.src.Loops = nil
			for i, group := range []struct {
				cold, noise bool
				count       int
			}{
				{false, false, 1}, {false, true, 2}, {true, false, 2}, {true, true, 2},
			} {
				key := fmt.Sprintf("shared-flags-%d", i)
				conv, err := f.Store.EnsureConversation(f.Source.ID, key, "Shared flags")
				requirements.NoError(err)
				for n := range group.count {
					f.add(f.now.Add(-time.Duration(60+i*5+n)*time.Minute), false, func(m *store.Message) {
						m.ConversationID = conv
						if group.noise {
							m.ListID = sql.NullString{String: "updates.example.com", Valid: true}
						}
					})
				}
				if group.cold {
					requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, key))
				}
			}
			f.project()
			baseline, err := Build(t.Context(), f.src, relationshipRequest(ref))
			requirements.NoError(err)
			requirements.NotNil(baseline.Person)
			requirements.Equal(1, baseline.Person.InteractionCount)
			if saved {
				assertions.Positive(baseline.Person.Person.PersonID)
			} else {
				assertions.Zero(baseline.Person.Person.PersonID)
			}
			for _, tc := range []struct {
				cold, noise                        bool
				entries, coldOmitted, noiseOmitted int
			}{
				{false, false, 1, 2, 2}, {false, true, 2, 4, 0},
				{true, false, 2, 0, 4}, {true, true, 4, 0, 0},
			} {
				t.Run(fmt.Sprintf("cold=%t/noise=%t", tc.cold, tc.noise), func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					req := relationshipRequest(ref)
					req.IncludeCold, req.IncludeNoise = tc.cold, tc.noise
					got, err := Build(t.Context(), f.src, req)
					requirements.NoError(err)
					assertions.Equal(baseline.Person, got.Person)
					assertions.Len(got.History, tc.entries)
					assertions.Equal(tc.coldOmitted, got.Omitted.Cold)
					assertions.Equal(tc.noiseOmitted, got.Omitted.Noise)
					req.Around = "2026-10-22T12:00:00Z"
					req.Before, req.After = tc.entries, 0
					req.AfterSet = true
					got, err = Build(t.Context(), f.src, req)
					requirements.NoError(err)
					assertions.Len(got.History, tc.entries)
					assertions.Equal(tc.coldOmitted, got.Omitted.Cold)
					assertions.Equal(tc.noiseOmitted, got.Omitted.Noise)
				})
			}
		})
	}
}

func TestColdConversationIncludesAllMessages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	conv, err := f.Store.EnsureConversation(f.Source.ID, "cold-conversation", "Cold conversation")
	requirements.NoError(err)
	requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, "cold-conversation"))
	ids := []int64{}
	for i := range 8 {
		ids = append(ids, f.add(f.now.Add(-time.Hour+time.Duration(i/2)*time.Minute), false, func(m *store.Message) { m.ConversationID = conv }))
	}
	f.project()
	req := Request{Around: messageRef(ids[2]), Conversation: true, Before: 2, After: 5, Scope: Scope{Owner: true}, Format: "json", Budget: 65536}
	hiddenReq := req
	hiddenReq.Before, hiddenReq.After = 0, 0
	hiddenReq.BeforeSet, hiddenReq.AfterSet = true, true
	hidden, err := Build(t.Context(), f.src, hiddenReq)
	requirements.NoError(err)
	requirements.Len(hidden.History, 1)
	assertions.True(hidden.Anchor.Filtered)
	assertions.Equal(8, hidden.Omitted.Cold)
	req.IncludeCold = true
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(got.History, 8)
	assertions.False(got.Anchor.Filtered)
	assertions.Zero(got.Omitted.Cold)
	want := []string{}
	for _, id := range ids {
		want = append(want, messageRef(id))
	}
	req.Budget = 2048
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
		}
		if got.Cursor == "" {
			break
		}
		req.Cursor = got.Cursor
	}
	assertions.Greater(pages, 1)
	assertions.Equal(want, refs)
}
