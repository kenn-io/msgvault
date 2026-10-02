package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// A missing sender and a merged participant are both valid archived shapes.
// The immutable From envelope must drive the repair, and another envelope
// carried by that same participant must not enter the affected set.
func TestIdentityTargetedRefreshUsesEnvelopeAndSource(t *testing.T) {
	for _, path := range []string{"single", "batch"} {
		t.Run(path, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			f := storetest.New(t)
			participant := f.EnsureParticipant("survivor@example.test", "Synthetic", "example.test")
			create := func(key, address string, sender sql.NullInt64) int64 {
				t.Helper()
				id, err := f.Store.PersistMessage(&store.MessagePersistData{
					Message:    &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: key, MessageType: "email", SenderID: sender},
					Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, DisplayNames: []string{"Synthetic"}, EmailAddresses: []string{address}}},
				})
				require.NoError(err)
				return id
			}
			target := create("forwarded-gmail-mask", "Mask@Example.test", sql.NullInt64{})
			unrelated := create("another-envelope", "other@example.test", sql.NullInt64{Int64: participant, Valid: true})
			// Deliberately stale unrelated data proves the runtime repair is targeted:
			// a source-wide refresh would correct this row too.
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET identity_is_from_me=TRUE,is_from_me=TRUE WHERE id=?`), unrelated)
			require.NoError(err)
			confirmations := []store.IdentityConfirmation{{Identifier: "mask@example.test", Signals: []string{"provider-alias"}}}
			switch path {
			case "single":
				err = f.Store.AddAccountIdentity(f.Source.ID, "mask@example.test", "manual")
			case "batch":
				_, err = f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, confirmations)
			}
			require.NoError(err)
			got, err := f.Store.GetMessageIsFromMe(target)
			require.NoError(err)
			assert.True(got, "envelope-only message is repaired in the confirmation transaction")
			got, err = f.Store.GetMessageIsFromMe(unrelated)
			require.NoError(err)
			assert.True(got, "unrelated message must not be recomputed")
			_, err = f.Store.RemoveAccountIdentity(f.Source.ID, "mask@example.test")
			require.NoError(err)
			got, err = f.Store.GetMessageIsFromMe(target)
			require.NoError(err)
			assert.False(got)
			got, err = f.Store.GetMessageIsFromMe(unrelated)
			require.NoError(err)
			assert.True(got, "removal is also targeted")
		})
	}
}

func TestIdentityTargetedRefreshMatchesDotlessEnvelopeAddress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f := storetest.New(t)
	participant := f.EnsureParticipant("mask@localhost", "Synthetic", "localhost")
	id, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message:    &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "dotless-envelope", MessageType: "email"},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, DisplayNames: []string{"Synthetic"}, EmailAddresses: []string{"mask@localhost"}}},
	})
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "mask@localhost", "manual"))
	got, err := f.Store.GetMessageIsFromMe(id)
	require.NoError(err)
	assert.True(got)
	_, err = f.Store.RemoveAccountIdentity(f.Source.ID, "mask@localhost")
	require.NoError(err)
	got, err = f.Store.GetMessageIsFromMe(id)
	require.NoError(err)
	assert.False(got)
}

func TestIdentityTargetedRefreshPreservesDatabaseCaseRules(t *testing.T) {
	require := require.New(t)

	f := storetest.New(t)
	participant := f.EnsureParticipant("survivor@example.test", "Synthetic", "example.test")
	address := "MÄSK@example.test"
	id, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message:    &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "unicode-envelope", MessageType: "email"},
		Recipients: []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{participant}, DisplayNames: []string{"Synthetic"}, EmailAddresses: []string{address}}},
	})
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, address, "manual"))
	got, err := f.Store.GetMessageIsFromMe(id)
	require.NoError(err)
	assert.True(t, got, "target lookup must use the same database case rules as ownership attribution")
}

func TestIdentityTargetedRefreshPreservesLegacyParticipantCaseRules(t *testing.T) {
	for _, location := range []string{"email", "identifier"} {
		for _, path := range []string{"single", "batch"} {
			t.Run(location+"/"+path, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				f := storetest.New(t)
				const address = "MÄSK@example.test"
				participantEmail := address
				if location == "identifier" {
					participantEmail = "survivor@example.test"
				}
				participant := f.EnsureParticipant(participantEmail, "Synthetic", "example.test")
				if location == "identifier" {
					require.NoError(f.Store.SetParticipantIdentifier(participant, "email", address))
					// Legacy identifiers are authoritative only without a primary email.
					_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE participants SET email_address=NULL WHERE id=?`), participant)
					require.NoError(err)
				}
				id, err := f.Store.PersistMessage(&store.MessagePersistData{
					Message: &store.Message{SourceID: f.Source.ID, ConversationID: f.ConvID,
						SourceMessageID: "unicode-legacy-sender", MessageType: "email",
						SenderID: sql.NullInt64{Int64: participant, Valid: true}},
				})
				require.NoError(err)
				got, err := f.Store.GetMessageIsFromMe(id)
				require.NoError(err)
				assert.False(got)
				confirmations := []store.IdentityConfirmation{{Identifier: address, Signals: []string{"provider-alias"}}}
				switch path {
				case "single":
					err = f.Store.AddAccountIdentity(f.Source.ID, address, "manual")
				case "batch":
					_, err = f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, confirmations)
				}
				require.NoError(err)
				got, err = f.Store.GetMessageIsFromMe(id)
				require.NoError(err)
				assert.True(got, "legacy sender lookup must share the ownership SQL case rules")
				_, err = f.Store.RemoveAccountIdentity(f.Source.ID, address)
				require.NoError(err)
				got, err = f.Store.GetMessageIsFromMe(id)
				require.NoError(err)
				assert.False(got)
			})
		}
	}
}
