package store_test

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// A conflicting sender needs an explicit recipient or room membership to resolve.
func TestResolvePersonReferenceSenderFallbackMatchesProjector(t *testing.T) {
	for _, saved := range []bool{false, true} {
		for _, scenario := range []struct {
			name            string
			explicitFrom    bool
			addressed       bool
			senderRecipient bool
			senderMember    bool
			wantSender      bool
		}{
			{"conflicting_from", true, true, false, false, false},
			{"no_from", false, true, false, false, true},
			{"explicit_recipient", true, true, true, false, true},
			{"room_member", true, false, false, true, true},
			{"addressed_suppresses_room", true, true, false, true, false},
		} {
			t.Run(scenario.name+"/saved="+strconv.FormatBool(saved), func(t *testing.T) {
				requirements, assertions := require.New(t), assert.New(t)
				f := storetest.New(t)
				sender := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
				from := f.EnsureParticipant("author@example.com", "Author", "example.com")
				to := f.EnsureParticipant("to@example.com", "To", "example.com")
				cc := f.EnsureParticipant("cc@example.com", "Cc", "example.com")
				bcc := f.EnsureParticipant("bcc@example.com", "Bcc", "example.com")
				room := f.EnsureParticipant("room@example.com", "Room member", "example.com")
				if saved {
					for _, id := range []int64{sender, from, to, cc, bcc, room} {
						_, _, err := f.Store.CreatePersonFromParticipant(id)
						requirements.NoError(err)
					}
				}
				m := f.NewMessage().WithSentAt(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)).Build()
				m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
				id, err := f.Store.UpsertMessage(m)
				requirements.NoError(err)
				if scenario.explicitFrom {
					requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{from}, []string{"Author"}))
				}
				if scenario.addressed {
					for role, participant := range map[string]int64{"to": to, "cc": cc, "bcc": bcc} {
						requirements.NoError(f.Store.ReplaceMessageRecipients(id, role, []int64{participant}, []string{role}))
					}
				}
				if scenario.senderRecipient {
					requirements.NoError(f.Store.ReplaceMessageRecipients(id, "cc", []int64{cc, sender}, []string{"Cc", "Sender"}))
				}
				requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, room, "member"))
				if scenario.senderMember {
					requirements.NoError(f.Store.EnsureConversationParticipant(f.ConvID, sender, "member"))
				}
				ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
				for participant, visible := range map[int64]bool{
					sender: scenario.wantSender, from: scenario.explicitFrom,
					to: scenario.addressed, cc: scenario.addressed, bcc: scenario.addressed,
					room: !scenario.addressed,
				} {
					got, resolveErr := f.Store.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(participant, 10), nil, 10)
					requirements.NoError(resolveErr)
					if visible {
						requirements.Len(got, 1)
						assertions.Equal([]int64{participant}, got[0].ParticipantIDs)
					} else {
						assertions.Empty(got, "excluded participant %d must not resolve", participant)
					}
				}
			})
		}
	}
}

func TestResolvePersonReferenceSuppressedSenderCannotBridgeCluster(t *testing.T) {
	for _, tc := range []struct {
		name              string
		suppressed, saved bool
	}{
		{name: "unobserved bridge"},
		{name: "suppressed sender", suppressed: true},
		{name: "saved suppressed sender", suppressed: true, saved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			f := storetest.New(t)
			visible := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
			other := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
			hidden := f.EnsureParticipant("hidden@example.com", "Hidden", "example.com")
			if tc.suppressed {
				m := f.NewMessage().WithSentAt(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)).Build()
				m.SenderID = sql.NullInt64{Int64: hidden, Valid: true}
				id, err := f.Store.UpsertMessage(m)
				requirements.NoError(err)
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{visible}, []string{"Visible"}))
				if !tc.saved {
					requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{other}, []string{"Other"}))
				}
			} else {
				for _, participant := range []int64{visible, other} {
					m := f.NewMessage().WithSentAt(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)).Build()
					m.SenderID = sql.NullInt64{Int64: participant, Valid: true}
					_, err := f.Store.UpsertMessage(m)
					requirements.NoError(err)
				}
			}
			ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID})
			before, err := f.Store.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(visible, 10), nil, 10)
			requirements.NoError(err)
			requirements.Len(before, 1)
			assertions.Equal([]int64{visible}, before[0].ParticipantIDs)
			_, err = f.Store.LinkParticipants(visible, hidden)
			requirements.NoError(err)
			if tc.saved {
				person, _, err := f.Store.CreatePersonFromParticipant(visible)
				requirements.NoError(err)
				got, err := f.Store.ResolvePersonReferenceContext(ctx, "person", strconv.FormatInt(person.ID, 10), nil, 10)
				requirements.NoError(err)
				requirements.Len(got, 1)
				assertions.Equal([]int64{visible}, got[0].ParticipantIDs)
				return
			}
			_, err = f.Store.LinkParticipants(hidden, other)
			requirements.NoError(err)
			after, err := f.Store.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(visible, 10), nil, 10)
			requirements.NoError(err)
			assertions.Equal(before, after)
			got, err := f.Store.ResolvePersonReferenceContext(ctx, "participant", strconv.FormatInt(other, 10), nil, 10)
			requirements.NoError(err)
			requirements.Len(got, 1)
			assertions.Equal([]int64{other}, got[0].ParticipantIDs)
		})
	}
}
