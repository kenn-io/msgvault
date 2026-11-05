package store_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
)

// Missing staged policy must not erase an ordinarily imported chat. Explicit
// cold state must still suppress its projected rows before queue retraction.
func TestActivityReadersDefaultMissingPolicyToFull(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	src, err := f.Store.GetOrCreateSource("beeper", "synthetic-account")
	requirements.NoError(err)
	requirements.NoError(f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	conv, err := f.Store.EnsureConversation(src.ID, "synthetic-room", "Room")
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='group_chat' WHERE id=?`), conv)
	requirements.NoError(err)
	id := f.add(t, conv, src.ID, timelineContactQuery(f).Now, false, "")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='chat' WHERE id=?`), id)
	requirements.NoError(err)
	projectTimelineActivity(t, f, "UTC")
	var projected int
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM activity_events WHERE message_id=?`), id).Scan(&projected))
	requirements.Equal(1, projected)
	for _, cold := range []bool{false, true} {
		if cold {
			requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), src.ID, "synthetic-room", conversationpolicy.Cold))
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversation_policy SET effective_tier='cold' WHERE conversation_id=?`), conv)
			requirements.NoError(err)
		}
		want := 1
		if cold {
			want = 0
		}
		bursts, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
		requirements.NoError(err)
		assertions.Len(bursts.Bursts, want)
		messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: conv, Limit: 10})
		requirements.NoError(err)
		assertions.Len(messages, want)
		display, err := f.Store.MessageDisplayContext(t.Context(), []int64{id})
		requirements.NoError(err)
		assertions. // Display hydrates already-authorized cold entries too.
				Len(display, 1)
		summary, err := f.Store.MessageContactAggregateContext(t.Context(), timelineContactQuery(f))
		requirements.NoError(err)
		assertions.Equal(want, summary.Count)
		latest, err := f.Store.LatestEligibleMessageContext(t.Context(), conv)
		requirements.NoError(err)
		if cold {
			assertions.Zero(latest)
		} else {
			assertions.Equal(id, latest)
		}
	}
}

// The projection's audience policy also governs live readers and saved aliases.
func TestActivityReadersAudienceUsesProjectionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		out, saved, aliases, otherOwner bool
	}{
		{name: "inbound extra recipient"},
		{name: "inbound saved aliases", saved: true, aliases: true},
		{name: "outbound unsaved", out: true},
		{name: "outbound saved", out: true, saved: true},
		{name: "inbound other account owner", saved: true, otherOwner: true},
		{name: "outbound other account owner", out: true, saved: true, otherOwner: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			f := newTimelineActivityFixture(t)
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			if tc.otherOwner {
				src, err := f.Store.GetOrCreateSource("gmail", "bob@example.com")
				require.NoError(err)
				require.NoError(f.Store.AddAccountIdentity(src.ID, "bob@example.com", "manual"))
			}
			if tc.aliases {
				_, err := f.Store.LinkParticipants(f.peer, other)
				require.NoError(err)
			}
			var personID int64
			if tc.saved {
				person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				require.NoError(err)
				personID = person.ID
			}
			id := f.add(t, f.ConvID, f.Source.ID, tierAsOf, tc.out, "")
			to := []int64{f.owner, other}
			if tc.out {
				to = []int64{f.peer, other}
			}
			require.NoError(f.Store.ReplaceMessageRecipients(id, "to", to, nil))
			projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 100, MaxDirectCounterparts: 1})
			require.NoError(err)
			_, err = projector.RunOnce(t.Context())
			require.NoError(err)
			want := 0
			if tc.aliases {
				want = 1
			}
			q := timelineBurstQuery()
			if tc.otherOwner {
				q.Subject = store.ActivitySubject{PersonID: personID}
			}
			page, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			require.NoError(err)
			assert.Len(page.Bursts, want)
			q.Filters.IncludeNoise = true
			page, err = f.Store.ListActivityBurstsContext(t.Context(), q)
			require.NoError(err)
			require.Len(page.Bursts, 1)
			wantInbound, wantOutbound := want, 0
			if tc.out {
				wantInbound, wantOutbound = 0, want
			}
			assert.Equal(wantInbound, page.Bursts[0].Inbound)
			assert.Equal(wantOutbound, page.Bursts[0].Outbound)
			found := false
			for _, cp := range page.Bursts[0].Counterparts {
				if cp.ParticipantID == f.peer {
					found = true
					assert.Equal(tc.aliases, cp.Counted)
					if tc.otherOwner && tc.out {
						assert.Equal(store.EvidenceCoPresence, cp.Evidence)
					}
				}
			}
			assert.True(found)
			contact := timelineContactQuery(f)
			contact.Now = tierAsOf
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), contact)
			require.NoError(err)
			assert.Equal(want, aggregate.Count)
			if tc.otherOwner {
				tier, err := f.Store.ExplainPersonTierContext(t.Context(), personID, tierOptions())
				require.NoError(err)
				assert.Equal([2]int{wantInbound, wantOutbound}, [2]int{tier.Signals.InboundLong, tier.Signals.OutboundLong})
			}
			if tc.otherOwner && tc.out {
				messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: f.ConvID, Limit: 10, Filters: q.Filters})
				require.NoError(err)
				require.Len(messages, 1)
				for _, cp := range messages[0].Counterparts {
					if cp.ParticipantID == f.peer {
						assert.Equal(store.EvidenceCoPresence, cp.Evidence)
						assert.False(cp.Counted)
					}
				}
			}
		})
	}
}

// An explicit chat envelope overrides a larger room roster; without envelope
// recipients, the classifier uses that roster and still excludes owner aliases.
func TestActivityReadersChatAudienceUsesEnvelopeOrRoster(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		t.Run(strconv.FormatBool(envelope), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			src, err := f.Store.GetOrCreateSource("beeper", "synthetic-account")
			requirements.NoError(err)
			requirements.NoError(f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
			conv, err := f.Store.EnsureConversation(src.ID, "audience-room", "Room")
			requirements.NoError(err)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='group_chat' WHERE id=?`), conv)
			requirements.NoError(err)
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			for _, id := range []int64{f.owner, f.peer, other} {
				requirements.NoError(f.Store.EnsureConversationParticipant(conv, id, "member"))
			}
			id := f.add(t, conv, src.ID, timelineContactQuery(f).Now, false, "")
			if !envelope {
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", nil, nil))
			}
			projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 100, MaxDirectCounterparts: 1})
			requirements.NoError(err)
			_, err = projector.RunOnce(t.Context())
			requirements.NoError(err)
			q := timelineBurstQuery()
			q.Filters = store.ActivityFilters{IncludeNoise: true}
			got, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(got.Bursts, 1)
			want := 0
			if envelope {
				want = 1
			}
			assertions.Equal(want, got.Bursts[0].Inbound)
		})
	}
}
