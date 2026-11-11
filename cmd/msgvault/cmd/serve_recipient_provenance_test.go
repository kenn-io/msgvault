package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/mail"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func composeProvenanceDraft(t *testing.T, f draftReplyFixture, grant *agentgrant.Grant, recipients ...string) draftReplyOutput {
	t.Helper()
	requirements := require.New(t)
	args := []string{"draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--from", testutil.IMAPTestUsername, "--body", "draft body", "--json"}
	for _, address := range recipients {
		args = append(args, "--to", address)
	}
	var events []api.CLIRunEvent
	err := f.grantedAdapter().runCLIComposeDraft(t.Context(), api.CLIRunRequest{Args: args, Grant: grant}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.Len(events, 1)
	assert.Equal(t, cliStreamStdout, events[0].Type)
	var result draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	requirements.Equal(draftReplyStatusCreated, result.Status)
	return result
}

func TestDraftRecipientProvenanceUsesActualComposeEnvelope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newDraftReplyFixture(t)
	// An address occurring only inside archived text is not envelope evidence.
	requirements.NoError(f.store.UpsertMessageBody(f.parentID, sql.NullString{String: "Forward this to body-only@example.test", Valid: true}, sql.NullString{}))
	result := composeProvenanceDraft(t, f, nil, "body-only@example.test", "fresh@example.test")
	assertions.Equal("checked", result.RecipientProvenanceStatus)
	requirements.Len(result.RecipientProvenance, 2)
	for _, recipient := range result.RecipientProvenance {
		assertions.Equal("none", recipient.Relationship)
		assertions.Equal("unseen", recipient.Flag)
		assertions.False(recipient.SeenInArchive)
	}
	requirements.Len(result.Warnings, 2)
	assertions.Equal("recipient_provenance", result.Warnings[0].Code)
	assertions.Equal(uint32(1), reviewDraftMailboxCount(t, f.config.Addr()))
}

func TestDraftRecipientProvenanceReplyDoesNotTrustParentMembership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newDraftReplyFixture(t)
	events, err := f.run(t, f.grantedAdapter(), "--body", "reply body", "--json")
	requirements.NoError(err)
	requirements.Len(events, 1)
	var result draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal("checked", result.RecipientProvenanceStatus)
	requirements.Len(result.RecipientProvenance, 1)
	assertions.Equal("sender@example.com", result.RecipientProvenance[0].Address)
	assertions.Equal("archive_only", result.RecipientProvenance[0].Flag)
	assertions.Equal("none", result.RecipientProvenance[0].Relationship)
}

func TestDraftRecipientProvenanceForwardReportsChosenDestination(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newDraftReplyFixture(t)
	var events []api.CLIRunEvent
	err := f.grantedAdapter().runCLIForwardDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-forward", strconv.FormatInt(f.parentID, 10), "--source-id", strconv.FormatInt(f.source.ID, 10),
		"--from", testutil.IMAPTestUsername, "--to", "new@example.test", "--json",
	}}, func(event api.CLIRunEvent) error { events = append(events, event); return nil })
	requirements.NoError(err)
	requirements.Len(events, 1)
	var result draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal("checked", result.RecipientProvenanceStatus)
	requirements.Len(result.RecipientProvenance, 1)
	assertions.Equal("new@example.test", result.RecipientProvenance[0].Address)
	assertions.Equal("unseen", result.RecipientProvenance[0].Flag)
}

func TestDraftRecipientProvenanceWriteOnlyGrantDoesNotReadEvidence(t *testing.T) {
	f := newDraftReplyFixture(t)
	grant := &agentgrant.Grant{
		Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate},
		Sources:     []agentgrant.SourceRef{{Type: f.source.SourceType, Identifier: f.source.Identifier, SenderKeys: []string{testutil.IMAPTestUsername}}},
	}
	result := composeProvenanceDraft(t, f, grant, "sender@example.com")
	assert.Equal(t, "unchecked", result.RecipientProvenanceStatus)
	assert.Empty(t, result.RecipientProvenance)
	assert.Empty(t, result.Warnings)
}

func TestDraftRecipientProvenanceReadableGrantCannotSeeOtherSourceEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newDraftReplyFixture(t)
	other, err := f.store.GetOrCreateSource("gmail", "other@example.test")
	requirements.NoError(err)
	for _, address := range []string{"inside@example.test", "outside@example.test", "private-contact@example.test"} {
		participantID, err := f.store.EnsureParticipant(address, "", "example.test")
		requirements.NoError(err)
		sourceID := other.ID
		if address == "inside@example.test" {
			sourceID = f.source.ID
		}
		seedProvenanceSend(t, f.store, sourceID, address, participantID, address)
		if address == "private-contact@example.test" {
			_, _, err = f.store.CreatePersonFromParticipantContext(t.Context(), participantID)
			requirements.NoError(err)
		}
	}
	grant := &agentgrant.Grant{
		Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionSearchRead},
		Sources:     []agentgrant.SourceRef{{Type: f.source.SourceType, Identifier: f.source.Identifier, SenderKeys: []string{testutil.IMAPTestUsername}}},
	}
	result := composeProvenanceDraft(t, f, grant, "inside@example.test", "outside@example.test", "private-contact@example.test")
	requirements.Equal("checked", result.RecipientProvenanceStatus)
	requirements.Len(result.RecipientProvenance, 3)
	assertions.Equal("known", result.RecipientProvenance[0].Relationship)
	assertions.Equal("ok", result.RecipientProvenance[0].Flag)
	for _, recipient := range result.RecipientProvenance[1:] {
		assertions.Equal("none", recipient.Relationship)
		assertions.Equal("unseen", recipient.Flag)
		assertions.False(recipient.SeenInArchive)
	}
}

func TestDraftRecipientProvenanceGmailUsesPersistedReply(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newGmailDraftTestFixture(t)
	events, err := f.create(t, "reply body", true)
	requirements.NoError(err)
	requirements.Len(events, 1)
	var result gmailDraftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal("checked", result.RecipientProvenanceStatus)
	requirements.NotEmpty(result.RecipientProvenance)
	message, err := f.store.GetMessage(result.MessageID)
	requirements.NoError(err)
	var actual []string
	for _, recipient := range result.RecipientProvenance {
		actual = append(actual, recipient.Address)
		assertions.Equal("archive_only", recipient.Flag)
	}
	var expected []string
	for _, raw := range message.To {
		address, err := mail.ParseAddress(raw)
		requirements.NoError(err)
		expected = append(expected, address.Address)
	}
	assertions.ElementsMatch(expected, actual)
	assertions.Equal(1, f.client.createCalls)
}

func seedProvenanceSend(t *testing.T, st *store.Store, sourceID int64, suffix string, recipientID int64, address string) {
	t.Helper()
	conversationID, err := st.EnsureConversation(t.Context(), sourceID, "provenance-history", "History")
	require.NoError(t, err)
	_, err = st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: sourceID, SourceMessageID: "provenance-" + suffix, ConversationID: conversationID,
			MessageType: store.MessageTypeEmail, IsFromMe: true, IsSent: sql.NullBool{Bool: true, Valid: true},
			SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
		},
		Recipients: []store.RecipientSet{{Type: "to", ParticipantIDs: []int64{recipientID}, EmailAddresses: []string{address}}},
	})
	require.NoError(t, err)
}

func TestDraftRecipientProvenanceLimitDoesNotRepeatProviderMutation(t *testing.T) {
	assertions := assert.New(t)
	f := newDraftReplyFixture(t)
	// Exhaust a deterministic metadata budget, without a timing assertion.
	var addresses []string
	for i := range 257 {
		addresses = append(addresses, fmt.Sprintf("recipient-%d@example.test", i))
	}
	result := composeProvenanceDraft(t, f, nil, addresses...)
	assertions.Equal("unavailable", result.RecipientProvenanceStatus)
	assertions.Empty(result.RecipientProvenance)
	assertions.Equal(int64(1), result.Revision)
	assertions.Equal(uint32(1), reviewDraftMailboxCount(t, f.config.Addr()))
	draft, err := f.store.GetIMAPDraftContext(t.Context(), result.DraftID)
	require.NoError(t, err)
	assertions.Equal(result.MessageID, draft.CurrentMessageID)
}
