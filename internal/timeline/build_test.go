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

func TestBuildRejectsInvalidRequests(t *testing.T) {
	f := newTimelineFixture(t)
	for _, tc := range []struct {
		name    string
		request Request
		options []string
		want    error
	}{
		{"before without anchor", Request{Before: 1}, []string{"before/after", "around"}, ErrInvalidRequest},
		{"explicit zero after without anchor", Request{AfterSet: true}, []string{"before/after", "around"}, ErrInvalidRequest},
		{"relationship after", Request{With: "email:alice@example.com", After: 1}, []string{"before/after", "around"}, ErrInvalidRequest},
		{"relationship date", Request{With: "email:alice@example.com", Date: "2026-10-22"}, []string{"with", "date"}, ErrInvalidRequest},
		{"anchor date", Request{Around: "2026-10-22", Date: "2026-10-22"}, []string{"around", "date"}, ErrInvalidRequest},
		{"anchor malformed date", Request{Around: "2026-10-22", Date: "bad"}, []string{"around", "date"}, ErrInvalidRequest},
		{"day anchor", Request{Mode: ModeDay, Around: "msg:999999"}, []string{"mode=day", "around"}, ErrInvalidRequest},
		{"conversation without anchor", Request{Conversation: true}, []string{"conversation", "around"}, ErrInvalidRequest},
		{"anchor mode conversation without anchor", Request{Mode: ModeAnchor, Conversation: true}, []string{"conversation", "around"}, ErrInvalidRequest},
		{"relationship range", Request{With: "email:alice@example.com", From: "2026-10-21", To: "2026-10-22"}, nil, ErrInvalidRequest},
		{"relationship days", Request{With: "email:alice@example.com", Days: 7}, nil, ErrInvalidRequest},
		{"anchor days", Request{Around: "2026-10-22", Days: 7}, nil, ErrInvalidRequest},
		{"anchor range", Request{Around: "2026-10-22", From: "2026-10-21", To: "2026-10-22"}, nil, ErrInvalidRequest},
		{"brief days", Request{Brief: true, Days: 7}, nil, ErrInvalidRequest},
		{"brief range", Request{Brief: true, From: "2026-10-19", To: "2026-10-22"}, []string{"brief", "days/from/to"}, ErrInvalidRequest},
		{"too many days", Request{Days: 32}, nil, ErrWindowTooLarge},
		{"too many neighbours", Request{Before: 51}, nil, ErrInvalidRequest},
		{"negative neighbours", Request{After: -1}, nil, ErrInvalidRequest},
		{"negative budget", Request{Budget: -1}, nil, ErrInvalidRequest},
		{"budget below minimum", Request{Budget: 4095}, nil, ErrInvalidRequest},
		{"budget above maximum", Request{Budget: 65537}, nil, ErrInvalidRequest},
		{"invalid format", Request{Format: "html"}, nil, ErrInvalidRequest},
		{"invalid mode", Request{Mode: "invalid"}, nil, ErrInvalidRequest},
		{"invalid date", Request{Date: "bad"}, nil, ErrInvalidRequest},
		{"invalid tier", Request{Tiers: []int{4}}, nil, ErrInvalidRequest},
		{"date and days", Request{Date: "2026-10-22", Days: 4}, nil, ErrInvalidRequest},
		{"incomplete range", Request{From: "2026-10-22"}, nil, ErrInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			req := tc.request
			req.Scope = Scope{Owner: true}
			got, err := Build(t.Context(), f.src, req)
			requirements.ErrorIs(err, tc.want)
			assertions.Nil(got)
			for _, option := range tc.options {
				assertions.Contains(err.Error(), option)
			}
		})
	}
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
