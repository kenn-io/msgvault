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
	for _, provider := range []string{"beeper", "slack", "slackdump", "discord", "teams"} {
		t.Run(provider, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			src, err := f.Store.GetOrCreateSource(provider, "synthetic-account")
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
		})
	}
}

// Sender plus a non-owner recipient is a two-person audience, even when
// recipient-only counting sees just one. Saved aliases count as one person.
func TestActivityReadersInboundAudienceMatchesClassifier(t *testing.T) {
	for _, aliases := range []bool{false, true} {
		t.Run(strconv.FormatBool(aliases), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			id := f.add(t, f.ConvID, f.Source.ID, timelineContactQuery(f).Now, false, "")
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			if aliases {
				_, err := f.Store.LinkParticipants(f.peer, other)
				requirements.NoError(err)
				_, _, err = f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
			}
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner, other}, []string{"Owner", "Other"}))
			projectTimelineActivity(t, f, "UTC")
			cs, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
			requirements.NoError(err)
			requirements.Len(cs, 1)
			assertions.Equal(!aliases, store.ClassifyActivityCandidate(cs[0], 1).Broadcast)
			q := timelineBurstQuery()
			q.Filters = store.ActivityFilters{MaxDirect: 1, IncludeNoise: true}
			got, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(got.Bursts, 1)
			want := 0
			if aliases {
				want = 1
			}
			assertions.Equal(want, got.Bursts[0].Inbound)
			for _, cp := range got.Bursts[0].Counterparts {
				if cp.ParticipantID == f.peer {
					assertions.Equal(aliases, cp.Counted)
				}
			}
		})
	}
}

// An omitted reader limit follows projection policy for saved and unsaved
// contacts, rather than silently switching classification back to 25.
func TestActivityReadersOmittedLimitUsesProjectionPolicy(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(strconv.FormatBool(saved), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			if saved {
				_, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				requirements.NoError(err)
			}
			id := f.add(t, f.ConvID, f.Source.ID, timelineContactQuery(f).Now, true, "")
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer, other}, []string{"Alice", "Bob"}))
			projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 100, MaxDirectCounterparts: 1})
			requirements.NoError(err)
			_, err = projector.RunOnce(t.Context())
			requirements.NoError(err)
			got, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
			requirements.NoError(err)
			assertions.Empty(got.Bursts)
			q := timelineBurstQuery()
			q.Filters.IncludeNoise = true
			got, err = f.Store.ListActivityBurstsContext(t.Context(), q)
			requirements.NoError(err)
			requirements.Len(got.Bursts, 1)
			assertions.Zero(got.Bursts[0].Outbound)
			for _, cp := range got.Bursts[0].Counterparts {
				if cp.ParticipantID == f.peer {
					assertions.False(cp.Counted)
				}
			}
			summary, err := f.Store.MessageContactAggregateContext(t.Context(), timelineContactQuery(f))
			requirements.NoError(err)
			assertions.Zero(summary.Count)
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
			projectTimelineActivity(t, f, "UTC")
			cs, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
			requirements.NoError(err)
			requirements.Len(cs, 1)
			assertions.Equal(!envelope, store.ClassifyActivityCandidate(cs[0], 1).Broadcast)
			q := timelineBurstQuery()
			q.Filters = store.ActivityFilters{MaxDirect: 1, IncludeNoise: true}
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
