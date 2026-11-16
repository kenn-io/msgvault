package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func provenanceDraftFixture(t *testing.T) (*storetest.Fixture, int64, int64) {
	t.Helper()
	f := storetest.New(t)
	participantID := f.EnsureParticipant("recipient@example.test", "Recipient", "example.test")
	messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: f.Source.ID, ConversationID: f.ConvID, SourceMessageID: "draft",
			MessageType: store.MessageTypeEmail, IsFromMe: true,
			// Deliberately retain native sent evidence: explicit exclusion of
			// the current message must hold even after sent-copy deduplication.
			IsSent: sql.NullBool{Bool: true, Valid: true},
			SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
		},
		Recipients: []store.RecipientSet{
			{Type: "to", ParticipantIDs: []int64{participantID}, EmailAddresses: []string{"recipient@example.test"}},
			{Type: "cc", ParticipantIDs: []int64{participantID}, EmailAddresses: []string{"RECIPIENT@example.test"}},
		},
	})
	require.NoError(t, err)
	return f, messageID, participantID
}

func TestDraftRecipientProvenanceExcludesCurrentAndDeduplicatesEnvelope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, messageID, _ := provenanceDraftFixture(t)
	result, err := f.Store.DraftRecipientProvenanceContext(t.Context(), messageID, nil)
	requirements.NoError(err)
	requirements.Len(result, 1)
	assertions.Equal("recipient@example.test", result[0].Address)
	assertions.Equal("none", result[0].Relationship)
	assertions.Equal("unseen", result[0].Flag)
	assertions.False(result[0].SeenInArchive)
}

func TestDraftRecipientProvenanceCurrentDraftCannotBootstrapScopedContact(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, messageID, participantID := provenanceDraftFixture(t)
	_, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	result, err := f.Store.DraftRecipientProvenanceContext(t.Context(), messageID, []int64{f.Source.ID})
	requirements.NoError(err)
	requirements.Len(result, 1)
	assertions.Equal("none", result[0].Relationship)
	assertions.Equal("unseen", result[0].Flag)
	assertions.False(result[0].SeenInArchive)
}

func TestDraftRecipientProvenanceOwnerCanUseCuratedContactWithoutPriorMail(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, messageID, participantID := provenanceDraftFixture(t)
	_, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	result, err := f.Store.DraftRecipientProvenanceContext(t.Context(), messageID, nil)
	requirements.NoError(err)
	requirements.Len(result, 1)
	assertions.Equal("contact", result[0].Relationship)
	assertions.Equal("ok", result[0].Flag)
	assertions.Equal("curated_contact", result[0].Basis)
	assertions.False(result[0].SeenInArchive)
}

func TestDraftRecipientProvenanceDoesNotReplaceEnvelopeWithCurrentParticipant(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, messageID, participantID := provenanceDraftFixture(t)
	_, _, err := f.Store.CreatePersonFromParticipantContext(t.Context(), participantID)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE participants SET email_address = ? WHERE id = ?`), "different@example.test", participantID)
	requirements.NoError(err)
	result, err := f.Store.DraftRecipientProvenanceContext(t.Context(), messageID, nil)
	requirements.NoError(err)
	requirements.Len(result, 1)
	assertions.Equal("recipient@example.test", result[0].Address)
	assertions.Equal("none", result[0].Relationship)
	assertions.Equal("unseen", result[0].Flag)
}

func TestDraftRecipientProvenanceRejectsMissingSnapshotAndUnreadableEnvelope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f, messageID, _ := provenanceDraftFixture(t)
	for _, scope := range [][]int64{{}, {f.Source.ID + 100}} {
		result, err := f.Store.DraftRecipientProvenanceContext(t.Context(), messageID, scope)
		requirements.Error(err)
		assertions.Nil(result)
	}
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients SET email_address = NULL WHERE message_id = ?`), messageID)
	requirements.NoError(err)
	result, err := f.Store.DraftRecipientProvenanceContext(t.Context(), messageID, nil)
	requirements.Error(err)
	assertions.Nil(result)
}
