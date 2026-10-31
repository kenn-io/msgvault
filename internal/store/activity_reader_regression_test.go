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
			f := newTimelineActivityFixture(t)
			src, err := f.Store.GetOrCreateSource(provider, "synthetic-account")
			require.NoError(t, err)
			require.NoError(t, f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
			conv, err := f.Store.EnsureConversation(src.ID, "synthetic-room", "Room")
			require.NoError(t, err)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='group_chat' WHERE id=?`), conv)
			require.NoError(t, err)
			id := f.add(t, conv, src.ID, timelineContactQuery(f).Now, false, "")
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='chat' WHERE id=?`), id)
			require.NoError(t, err)
			projectTimelineActivity(t, f, "UTC")
			var projected int
			require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM activity_events WHERE message_id=?`), id).Scan(&projected))
			require.Equal(t, 1, projected)
			for _, cold := range []bool{false, true} {
				if cold {
					require.NoError(t, f.Store.SetConversationPolicyOverride(t.Context(), src.ID, "synthetic-room", conversationpolicy.Cold))
					_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversation_policy SET effective_tier='cold' WHERE conversation_id=?`), conv)
					require.NoError(t, err)
				}
				want := 1
				if cold {
					want = 0
				}
				bursts, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
				require.NoError(t, err)
				assert.Len(t, bursts.Bursts, want)
				messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: conv, Limit: 10})
				require.NoError(t, err)
				assert.Len(t, messages, want)
				display, err := f.Store.MessageDisplayContext(t.Context(), []int64{id})
				require.NoError(t, err)
				// Display hydrates already-authorized cold entries too.
				assert.Len(t, display, 1)
				summary, err := f.Store.MessageContactAggregateContext(t.Context(), timelineContactQuery(f))
				require.NoError(t, err)
				assert.Equal(t, want, summary.Count)
				latest, err := f.Store.LatestEligibleMessageContext(t.Context(), conv)
				require.NoError(t, err)
				if cold {
					assert.Zero(t, latest)
				} else {
					assert.Equal(t, id, latest)
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
			f := newTimelineActivityFixture(t)
			id := f.add(t, f.ConvID, f.Source.ID, timelineContactQuery(f).Now, false, "")
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			if aliases {
				_, err := f.Store.LinkParticipants(f.peer, other)
				require.NoError(t, err)
				_, _, err = f.Store.CreatePersonFromParticipant(f.peer)
				require.NoError(t, err)
			}
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner, other}, []string{"Owner", "Other"}))
			projectTimelineActivity(t, f, "UTC")
			cs, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
			require.NoError(t, err)
			require.Len(t, cs, 1)
			assert.Equal(t, !aliases, store.ClassifyActivityCandidate(cs[0], 1).Broadcast)
			q := timelineBurstQuery()
			q.Filters = store.ActivityFilters{MaxDirect: 1, IncludeNoise: true}
			got, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			require.NoError(t, err)
			require.Len(t, got.Bursts, 1)
			want := 0
			if aliases {
				want = 1
			}
			assert.Equal(t, want, got.Bursts[0].Inbound)
			for _, cp := range got.Bursts[0].Counterparts {
				if cp.ParticipantID == f.peer {
					assert.Equal(t, aliases, cp.Counted)
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
			f := newTimelineActivityFixture(t)
			if saved {
				_, _, err := f.Store.CreatePersonFromParticipant(f.peer)
				require.NoError(t, err)
			}
			id := f.add(t, f.ConvID, f.Source.ID, timelineContactQuery(f).Now, true, "")
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer, other}, []string{"Alice", "Bob"}))
			projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 100, MaxDirectCounterparts: 1})
			require.NoError(t, err)
			_, err = projector.RunOnce(t.Context())
			require.NoError(t, err)
			got, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
			require.NoError(t, err)
			assert.Empty(t, got.Bursts)
			q := timelineBurstQuery()
			q.Filters.IncludeNoise = true
			got, err = f.Store.ListActivityBurstsContext(t.Context(), q)
			require.NoError(t, err)
			require.Len(t, got.Bursts, 1)
			assert.Zero(t, got.Bursts[0].Outbound)
			for _, cp := range got.Bursts[0].Counterparts {
				if cp.ParticipantID == f.peer {
					assert.False(t, cp.Counted)
				}
			}
			summary, err := f.Store.MessageContactAggregateContext(t.Context(), timelineContactQuery(f))
			require.NoError(t, err)
			assert.Zero(t, summary.Count)
		})
	}
}

// An explicit chat envelope overrides a larger room roster; without envelope
// recipients, the classifier uses that roster and still excludes owner aliases.
func TestActivityReadersChatAudienceUsesEnvelopeOrRoster(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		t.Run(strconv.FormatBool(envelope), func(t *testing.T) {
			f := newTimelineActivityFixture(t)
			src, err := f.Store.GetOrCreateSource("beeper", "synthetic-account")
			require.NoError(t, err)
			require.NoError(t, f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
			conv, err := f.Store.EnsureConversation(src.ID, "audience-room", "Room")
			require.NoError(t, err)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE conversations SET conversation_type='group_chat' WHERE id=?`), conv)
			require.NoError(t, err)
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			for _, id := range []int64{f.owner, f.peer, other} {
				require.NoError(t, f.Store.EnsureConversationParticipant(conv, id, "member"))
			}
			id := f.add(t, conv, src.ID, timelineContactQuery(f).Now, false, "")
			if !envelope {
				require.NoError(t, f.Store.ReplaceMessageRecipients(id, "to", nil, nil))
			}
			projectTimelineActivity(t, f, "UTC")
			cs, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{id})
			require.NoError(t, err)
			require.Len(t, cs, 1)
			assert.Equal(t, !envelope, store.ClassifyActivityCandidate(cs[0], 1).Broadcast)
			q := timelineBurstQuery()
			q.Filters = store.ActivityFilters{MaxDirect: 1, IncludeNoise: true}
			got, err := f.Store.ListActivityBurstsContext(t.Context(), q)
			require.NoError(t, err)
			require.Len(t, got.Bursts, 1)
			want := 0
			if envelope {
				want = 1
			}
			assert.Equal(t, want, got.Bursts[0].Inbound)
		})
	}
}
