package store_test

import (
	"fmt"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestCleartextMarkerIsOrdinaryMessage(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			src, err := f.Store.GetOrCreateSource("matrix", "@owner:example.com")
			requirements.NoError(err)
			requirements.NoError(f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
			conv, err := f.Store.EnsureConversation(src.ID, "!room:example.com", "Room")
			requirements.NoError(err)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='direct_chat' WHERE id=?`), conv)
			requirements.NoError(err)
			person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
			requirements.NoError(err)
			var ids []int64
			for i := range 3 {
				id := f.add(t, conv, src.ID, tierAsOf.AddDate(0, 0, i-3), i == 1 && !pending || i != 1 && pending, "")
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='matrix',snippet=? WHERE id=?`), store.MatrixEncryptedPlaceholder, id)
				requirements.NoError(err)
				if pending && i > 0 {
					_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO matrix_encrypted_events(message_id,conversation_id,session_id,ciphertext) VALUES(?,?,?,?)`), id, conv, "session", "opaque")
					requirements.NoError(err)
					_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM message_bodies WHERE message_id=?`), id)
					requirements.NoError(err)
				}
				ids = append(ids, id)
			}
			projectTierSignals(t, f.Fixture, "UTC", 0)
			explanation, err := f.Store.ExplainPersonTierContext(t.Context(), person.ID, tierOptions())
			requirements.NoError(err)
			if pending {
				assertions.Equal(3, explanation.Tier)
				assertions.Equal(1, explanation.Signals.OutboundLong)
				assertions.Zero(explanation.Signals.InboundLong)
				assertions.Equal(1, explanation.Signals.ActiveDaysLong)
				assertions.Equal(tierAsOf.AddDate(0, 0, -3).Format(time.DateOnly), explanation.Signals.LastDate)
				requirements.NotNil(explanation.LastContactDate)
				assertions.Equal(tierAsOf.AddDate(0, 0, -3).Format(time.DateOnly), *explanation.LastContactDate)
			} else {
				assertions.Equal(2, explanation.Tier)
				assertions.Equal(3, explanation.Signals.ActiveDaysLong)
			}
			if !pending {
				display, err := f.Store.MessageDisplayContext(t.Context(), ids)
				requirements.NoError(err)
				for _, id := range ids {
					assertions.False(display[id].Encrypted)
					assertions.Equal(store.MatrixEncryptedPlaceholder, display[id].Snippet)
				}
			}
			q := timelineContactQuery(f)
			q.Now = tierAsOf
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			requirements.NoError(err)
			if pending {
				assertions.Equal(1, aggregate.Count)
				requirements.NotNil(aggregate.LastAt)
				assertions.Equal(tierAsOf.AddDate(0, 0, -3), *aggregate.LastAt)
			} else {
				assertions.Equal(3, aggregate.Count)
			}
			bq := timelineBurstQuery()
			bq.Filters.IncludeNoise = true
			bursts, err := f.Store.ListActivityBurstsContext(t.Context(), bq)
			requirements.NoError(err)
			requirements.Len(bursts.Bursts, 3)
			for i, burst := range bursts.Bursts {
				assertions.Equal(tierAsOf.AddDate(0, 0, i-3).Format(time.DateOnly), burst.LocalDate)
				inbound, outbound := 1, 0
				if i == 1 {
					inbound, outbound = 0, 1
				}
				if pending {
					inbound, outbound = 0, 0
					if i == 0 {
						outbound = 1
					}
				}
				assertions.Equal(inbound, burst.Inbound)
				assertions.Equal(outbound, burst.Outbound)
				requirements.NotEmpty(burst.Counterparts)
				if pending && i > 0 {
					for _, cp := range burst.Counterparts {
						assertions.False(cp.Counted)
					}
				}
			}
		})
	}
}

func TestMissingIDCannotCollideWithRealID(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	id := f.add(t, f.ConvID, f.Source.ID, q.Now.Add(-time.Hour), false, "")
	f.add(t, f.ConvID, f.Source.ID, q.Now, true, fmt.Sprintf("id:%d", id))
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(2, got.Count)
	bursts, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	requirements.NoError(err)
	requirements.Len(bursts.Bursts, 1)
	assertions.Equal(1, bursts.Bursts[0].Inbound)
	assertions.Equal(1, bursts.Bursts[0].Outbound)
}

func TestPromotionPreservesLinkedContactCounts(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	alias := f.EnsureParticipant("noreply@example.com", "Alias", "example.com")
	_, err := f.Store.LinkParticipants(f.peer, alias)
	requirements.NoError(err)
	q := timelineContactQuery(f)
	q.Subject.ParticipantIDs = []int64{f.peer, alias}
	f.peer = alias
	f.add(t, f.ConvID, f.Source.ID, q.Now, false, "")
	projectTierSignals(t, f.Fixture, "UTC", 0)
	before, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(1, before.Count)
	person, _, err := f.Store.CreatePersonFromParticipant(alias)
	requirements.NoError(err)
	q.Subject = store.ActivitySubject{PersonID: person.ID}
	unprojected, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(before, unprojected)
	projectTierSignals(t, f.Fixture, "UTC", 0)
	after, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(before, after)
	bursts, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	requirements.NoError(err)
	requirements.Len(bursts.Bursts, 1)
	assertions.Equal(1, bursts.Bursts[0].Inbound)
}
