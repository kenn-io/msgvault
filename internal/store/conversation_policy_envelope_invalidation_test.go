package store_test

import (
	"database/sql"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicyEnvelopeWithdrawalInvalidatesSharedContext(t *testing.T) {
	for _, operation := range []string{"recipient replacement", "recipient name replacement", "recipient replay", "reader rollback", "persist recipients", "tombstone", "batch tombstone", "reader tombstone", "snapshot tombstone", "compatibility tombstone", "compatibility batch", "dedup", "move envelope", "migrate envelope"} {
		t.Run(operation, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s := testutil.NewTestStore(t)
			source, err := s.GetOrCreateSource("beeper", "envelope-invalidation")
			require.NoError(err)
			support, err := s.EnsureConversation(source.ID, "support", "Support")
			require.NoError(err)
			target, err := s.EnsureConversation(source.ID, "target", "Target")
			require.NoError(err)
			for _, room := range []string{"support", "target"} {
				require.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, room, conversationpolicy.Full))
			}
			pid, err := s.EnsureParticipantByIdentifier("beeper", "shared", "Envelope Name")
			require.NoError(err)
			envelope := &store.Message{SourceID: source.ID, ConversationID: support, SourceMessageID: "envelope", MessageType: "beeper"}
			envelopeID, err := s.UpsertMessage(envelope)
			require.NoError(err)
			require.NoError(s.ReplaceMessageRecipients(envelopeID, "from", []int64{pid}, []string{"Envelope Name"}))
			targetID, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: target, SourceMessageID: "target", MessageType: "beeper", SenderID: sql.NullInt64{Int64: pid, Valid: true}})
			require.NoError(err)
			// No current roster membership: historical senders still enter blocks.
			require.NoError(s.SetEmbedGen(t.Context(), []int64{targetID}, 73))
			var before int64
			require.NoError(s.DB().QueryRow(s.Rebind(`SELECT embedding_revision FROM messages WHERE id=?`), targetID).Scan(&before))
			var eligible bool
			require.NoError(s.DB().QueryRow(s.Rebind(`SELECT `+store.FullParticipantNameWhere("p.id", "p.display_name")+` FROM participants p WHERE id=?`), pid).Scan(&eligible))
			require.True(eligible)
			unchanged := operation == "recipient replay" || operation == "reader rollback"
			switch operation {
			case "recipient name replacement":
				err = s.ReplaceMessageRecipients(envelopeID, "from", []int64{pid}, []string{"Replacement Name"})
			case "recipient replay":
				err = s.ReplaceMessageRecipients(envelopeID, "from", []int64{pid}, []string{"Envelope Name"})
			case "reader rollback":
				err = s.MarkMessagesDeletedFromReader(source.ID, io.MultiReader(strings.NewReader("envelope\n"), iotest.ErrReader(errors.New("reader failed"))), 1)
			case "recipient replacement":
				err = s.ReplaceMessageRecipients(envelopeID, "from", nil, nil)
			case "persist recipients":
				_, err = s.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: envelope, Recipients: []store.RecipientSet{{Type: "from"}}})
			case "tombstone":
				err = s.MarkMessageDeleted(source.ID, "envelope")
			case "batch tombstone":
				err = s.MarkMessagesDeletedBatch(source.ID, []string{"envelope"})
			case "reader tombstone":
				err = s.MarkMessagesDeletedFromReader(source.ID, strings.NewReader("envelope\n"), 1)
			case "snapshot tombstone":
				_, err = s.ReconcileSourceMessageSnapshot(t.Context(), source.ID, map[string]struct{}{"target": {}})
			case "compatibility tombstone":
				err = s.MarkMessageDeletedBySourceMessageID(0, false, "envelope")
			case "compatibility batch":
				err = s.MarkMessagesDeletedBySourceMessageIDBatch(0, []string{"envelope"})
			case "move envelope":
				cold, createErr := s.EnsureConversation(source.ID, "cold", "Cold")
				require.NoError(createErr)
				envelope.ConversationID = cold
				_, err = s.UpsertMessage(envelope)
			case "migrate envelope":
				err = s.MigrateSourceMessageID(source.ID, support, "envelope", "target")
			case "dedup":
				_, err = s.MergeDuplicates(targetID, []int64{envelopeID}, "envelope-test")
			}
			if operation == "reader rollback" {
				require.ErrorContains(err, "reader failed")
			} else {
				require.NoError(err)
			}
			require.NoError(s.DB().QueryRow(s.Rebind(`SELECT `+store.FullParticipantNameWhere("p.id", "p.display_name")+` FROM participants p WHERE id=?`), pid).Scan(&eligible))
			assert.Equal(unchanged, eligible)
			var gen sql.NullInt64
			var revision int64
			require.NoError(s.DB().QueryRow(s.Rebind(`SELECT embed_gen,embedding_revision FROM messages WHERE id=?`), targetID).Scan(&gen, &revision))
			if unchanged {
				assert.Equal(sql.NullInt64{Int64: 73, Valid: true}, gen)
				assert.Equal(before, revision)
			} else {
				assert.False(gen.Valid, "a context carrying a withdrawn name must be rebuilt")
				assert.Greater(revision, before, "stale vectors must fail message revision visibility")
			}
		})
	}
}

func TestConversationPolicyRecipientAdditionInvalidatesSharedContext(t *testing.T) {
	for _, recipientType := range []string{"from", "to"} {
		for _, recipientCount := range []int{1, 301} {
			name := "one recipient"
			if recipientCount > 1 {
				name = "large recipient set"
			}
			t.Run(recipientType+"/"+name, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)

				s := testutil.NewTestStore(t)
				source, err := s.GetOrCreateSource("beeper", "recipient-addition-context")
				requirements.NoError(err)
				supportRoom, err := s.EnsureConversation(source.ID, "support", "Support")
				requirements.NoError(err)
				targetRoom, err := s.EnsureConversation(source.ID, "target", "Target")
				requirements.NoError(err)
				requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "support", conversationpolicy.Full))
				requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "target", conversationpolicy.Full))

				participantID, err := s.EnsureParticipantByIdentifier(
					"beeper", "synthetic:shared-recipient", "Synthetic Shared Recipient")
				requirements.NoError(err)
				requirements.NoError(s.EnsureConversationParticipantContext(t.Context(), targetRoom, participantID, "member"))

				supportMessageID, err := s.UpsertMessage(&store.Message{
					SourceID: source.ID, ConversationID: supportRoom,
					SourceMessageID: "support-envelope", MessageType: "beeper",
				})
				requirements.NoError(err)
				targetMessageID, err := s.UpsertMessage(&store.Message{
					SourceID: source.ID, ConversationID: targetRoom,
					SourceMessageID: "target-context", MessageType: "beeper",
				})
				requirements.NoError(err)
				requirements.NoError(s.SetEmbedGen(t.Context(), []int64{targetMessageID}, 73))

				var eligible bool
				requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT `+
					store.FullParticipantNameWhere("p.id", "p.display_name")+
					` FROM participants p WHERE p.id=?`), participantID).Scan(&eligible))
				assertions.False(eligible, "the name has no Full envelope support yet")
				var beforeRevision int64
				requirements.NoError(s.DB().QueryRow(s.Rebind(
					`SELECT embedding_revision FROM messages WHERE id=?`), targetMessageID).Scan(&beforeRevision))

				participantIDs := []int64{participantID}
				displayNames := []string{"Synthetic Shared Recipient"}
				for i := 1; i < recipientCount; i++ {
					fillerID, err := s.EnsureParticipantByIdentifier(
						"beeper", "synthetic:shared-recipient-"+strconv.Itoa(i), "Synthetic Filler")
					requirements.NoError(err)
					participantIDs = append(participantIDs, fillerID)
					displayNames = append(displayNames, "Synthetic Filler")
				}
				requirements.NoError(s.ReplaceMessageRecipients(
					supportMessageID, recipientType, participantIDs, displayNames))

				requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT `+
					store.FullParticipantNameWhere("p.id", "p.display_name")+
					` FROM participants p WHERE p.id=?`), participantID).Scan(&eligible))
				assertions.True(eligible, "the first Full envelope now supports the name")
				var afterGen sql.NullInt64
				var afterRevision int64
				requirements.NoError(s.DB().QueryRow(s.Rebind(
					`SELECT embed_gen,embedding_revision FROM messages WHERE id=?`), targetMessageID).Scan(&afterGen, &afterRevision))
				assertions.False(afterGen.Valid, "context that can now contain the newly eligible name must be rebuilt")
				assertions.Greater(afterRevision, beforeRevision, "in-flight contextual publication must fail its revision check")
			})
		}
	}
}
