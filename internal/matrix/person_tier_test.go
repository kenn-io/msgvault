package matrix

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func TestImportedEncryptedEventsDoNotPromotePersonTier(t *testing.T) {
	for _, encrypted := range []bool{true, false} {
		t.Run(fmt.Sprintf("encrypted=%t", encrypted), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
			require.NoError(err)
			require.NoError(st.AddAccountIdentity(source.ID, "@archive:example.org", "manual"))
			conv, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "direct_chat", "Example")
			require.NoError(err)
			owner, err := st.EnsureParticipantByIdentifier(SourceType, "@archive:example.org", "Archive")
			require.NoError(err)
			peer, err := st.EnsureParticipantByIdentifier(SourceType, "@member:example.org", "Member")
			require.NoError(err)
			require.NoError(st.ReplaceConversationParticipants(conv, []store.ConversationParticipantRef{
				{ParticipantID: owner, Role: "member"}, {ParticipantID: peer, Role: "member"},
			}))
			person, _, err := st.CreatePersonFromParticipant(peer)
			require.NoError(err)
			client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
			require.NoError(err)
			imp := NewImporter(st, &Runtime{Client: client})
			asOf := time.Now().UTC().Truncate(time.Millisecond)
			for i, sender := range []string{"@archive:example.org", "@member:example.org", "@archive:example.org"} {
				kind, content := "m.room.message", `{"msgtype":"m.text","body":"Synthetic contact"}`
				if encrypted && i > 0 {
					kind, content = "m.room.encrypted", `{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"opaque","session_id":"session","sender_key":"key"}`
				}
				evt := matrixTestEvent(t, fmt.Sprintf(`{"type":%q,"event_id":"$tier-%d","sender":%q,"origin_server_ts":%d,"content":%s}`,
					kind, i, sender, asOf.AddDate(0, 0, i-5).UnixMilli(), content))
				require.NoError(imp.persistEvent(t.Context(), source.ID, conv, evt, &ImportSummary{}))
			}
			ids, err := st.MessageExistsBatch(source.ID, []string{"$tier-0", "$tier-1", "$tier-2"})
			require.NoError(err)
			if encrypted {
				for _, key := range []string{"$tier-1", "$tier-2"} {
					messageID := ids[key]
					require.NotZero(messageID)
					var snippet string
					require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT snippet FROM messages WHERE id=?`), messageID).Scan(&snippet))
					assert.Equal(store.MatrixEncryptedPlaceholder, snippet)
					var pending bool
					require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT NOT recovered FROM matrix_encrypted_events WHERE message_id=?`), messageID).Scan(&pending))
					assert.True(pending)
					_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM message_bodies WHERE message_id=?`), messageID)
					require.NoError(err)
				}
			}
			projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC"})
			require.NoError(err)
			_, err = projector.RunOnce(t.Context())
			require.NoError(err)
			explanation, err := st.ExplainPersonTierContext(t.Context(), person.ID, store.PersonTierOptions{Config: persontiers.DefaultConfig()})
			require.NoError(err)
			bursts, err := st.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: asOf.AddDate(0, 0, -5).Format(time.DateOnly), ToDate: asOf.AddDate(0, 0, -3).Format(time.DateOnly), Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
			require.NoError(err)
			require.Len(bursts.Bursts, 3)
			for i, burst := range bursts.Bursts {
				assert.Equal(asOf.AddDate(0, 0, i-5).Format(time.DateOnly), burst.LocalDate)
				inbound, outbound := 0, 1
				if i == 1 {
					inbound, outbound = 1, 0
				}
				if encrypted && i > 0 {
					inbound, outbound = 0, 0
				}
				assert.Equal(inbound, burst.Inbound)
				assert.Equal(outbound, burst.Outbound)
				require.NotEmpty(burst.Counterparts)
				if encrypted && i > 0 {
					for _, counterpart := range burst.Counterparts {
						assert.False(counterpart.Counted)
					}
				}
			}
			aggregate, err := st.MessageContactAggregateContext(t.Context(), store.ContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{peer}}, Now: asOf, Location: time.UTC})
			require.NoError(err)
			wantCount, lastAt := 3, asOf.AddDate(0, 0, -3)
			if encrypted {
				wantCount, lastAt = 1, asOf.AddDate(0, 0, -5)
			}
			assert.Equal(wantCount, aggregate.Count)
			require.NotNil(aggregate.LastAt)
			assert.Equal(lastAt, *aggregate.LastAt)
			if encrypted {
				assert.Equal(3, explanation.Tier)
				assert.Equal(1, explanation.Signals.OutboundLong)
				assert.Zero(explanation.Signals.InboundLong)
				assert.Equal(1, explanation.Signals.ActiveDaysLong)
				assert.Equal(asOf.AddDate(0, 0, -5).Format(time.DateOnly), explanation.Signals.LastDate)
				require.NotNil(explanation.LastContactDate)
				assert.Equal(asOf.AddDate(0, 0, -5).Format(time.DateOnly), *explanation.LastContactDate)
			} else {
				assert.Equal(2, explanation.Tier)
				assert.Equal(2, explanation.Signals.OutboundLong)
				assert.Equal(1, explanation.Signals.InboundLong)
				assert.Equal(3, explanation.Signals.ActiveDaysLong)
				assert.Equal(asOf.AddDate(0, 0, -3).Format(time.DateOnly), explanation.Signals.LastDate)
			}
		})
	}
}
