package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// A missing sender still needs attribution through its original From envelope.
func TestIdentityTargetedRefreshUsesEnvelopeAndSource(t *testing.T) {
	for _, path := range []string{"single", "meeting", "unseen-batch"} {
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
			// A source-wide refresh would also correct this deliberately stale unrelated row.
			_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET identity_is_from_me=TRUE,is_from_me=TRUE WHERE id=?`), unrelated)
			require.NoError(err)
			var retry int64
			if path == "meeting" {
				retry = create("meeting-retry", "mask@example.test", sql.NullInt64{})
			}
			switch path {
			case "single":
				err = f.Store.AddAccountIdentity(f.Source.ID, "mask@example.test", "manual")
			case "meeting":
				err = f.Store.AddAccountIdentityAndRefreshMessageAttributionContext(t.Context(), f.Source.ID, "  mask@example.test  ", "meeting-owner", "meeting-retry")
			case "unseen-batch":
				outcomes, batchErr := f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, []store.IdentityConfirmation{{Identifier: "never-seen@example.test", Signals: []string{"manual"}}})
				require.NoError(batchErr)
				require.Len(outcomes, 1)
				assert.True(outcomes[0].Added)
			}
			require.NoError(err)
			if path != "unseen-batch" {
				got, err := f.Store.GetMessageIsFromMe(target)
				require.NoError(err)
				assert.True(got, "envelope-only message is repaired in the confirmation transaction")
			}
			if retry != 0 {
				got, err := f.Store.GetMessageIsFromMe(retry)
				require.NoError(err)
				assert.False(got, "the meeting being retried stays on its persistence path")
			}
			got, err := f.Store.GetMessageIsFromMe(unrelated)
			require.NoError(err)
			assert.True(got, "unrelated message must not be recomputed")
			if path != "single" {
				return
			}
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
