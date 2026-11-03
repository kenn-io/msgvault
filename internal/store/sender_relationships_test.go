package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func senderRelationshipMessage(t *testing.T, f *storetest.Fixture, outbound bool, role string, recipients ...string) int64 {
	t.Helper()
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	message := f.NewMessage().WithSentAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)).Build()
	message.IsFromMe = outbound
	message.IsSent = sql.NullBool{Bool: true, Valid: true}
	message.SenderID = sql.NullInt64{Int64: owner, Valid: true}
	if !outbound && len(recipients) > 0 {
		message.SenderID.Int64 = f.EnsureParticipant(recipients[0], "Sender", "example.com")
	}
	var ids []int64
	for _, address := range recipients {
		ids = append(ids, f.EnsureParticipant(address, "Recipient", "example.com"))
	}
	id, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: message,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{message.SenderID.Int64}, EmailAddresses: []string{map[bool]string{true: "owner@example.com", false: recipients[0]}[outbound]}},
			{Type: role, ParticipantIDs: ids, EmailAddresses: recipients},
		},
	})
	require.NoError(t, err)
	return id
}

func TestSenderRelationshipsExactEvidenceAndFreshness(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	senderRelationshipMessage(t, f, true, "to", "known@example.com")
	senderRelationshipMessage(t, f, true, "bcc", "private@example.com")
	senderRelationshipMessage(t, f, false, "to", "incoming@example.com")
	addresses := []string{" KNOWN@EXAMPLE.COM ", "known@example.com", "private@example.com", "incoming@example.com", "new@example.com"}
	got, err := f.Store.SenderRelationships(t.Context(), addresses, nil)
	requirements.NoError(err)
	assertions.Len(got, 4)
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "known", SeenInArchive: true}, got["known@example.com"])
	assertions.Equal("known", got["private@example.com"].Relationship)
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none", SeenInArchive: true}, got["incoming@example.com"])
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none"}, got["new@example.com"])
	participant := f.EnsureParticipant("incoming@example.com", "Sender", "example.com")
	_, _, err = f.Store.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	got, err = f.Store.SenderRelationships(t.Context(), []string{"incoming@example.com"}, nil)
	requirements.NoError(err)
	assertions.Equal("contact", got["incoming@example.com"].Relationship, "promotion applies without reindexing")
}

func TestSenderRelationshipsLegacyRawImportIsNotNativeOwnerSend(t *testing.T) {
	for _, sourceMessageID := range []string{
		"mbox-" + strings.Repeat("a", 64) + "-1",
		"eml-" + strings.Repeat("b", 64),
		"maildir-" + strings.Repeat("c", 64),
		"emlx-" + strings.Repeat("d", 64),
		"pst-abcdef123456-12345",
		"pst-12345", // legacy PST IDs used the decimal EntryID without an archive fingerprint.
	} {
		t.Run(sourceMessageID[:strings.IndexByte(sourceMessageID, '-')], func(t *testing.T) {
			f := storetest.New(t)
			id := senderRelationshipMessage(t, f, true, "to", "target@example.com")
			// Before importer ownership attribution was separated, matching the
			// configured owner address could set source_is_from_me on raw imports.
			// Source type is configurable, so a provider name cannot prove native origin.
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_message_id=? WHERE id=?`), sourceMessageID, id)
			require.NoError(t, err)
			got, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
			require.NoError(t, err)
			assert.Equal(t, store.SenderRelationshipEvidence{Relationship: "none", SeenInArchive: true}, got["target@example.com"])
		})
	}
}

func TestSenderRelationshipsRejectsLegacyMicrosoftAddressOwnership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	id := senderRelationshipMessage(t, f, false, "to", "target@example.test")
	msmail, err := f.Store.GetOrCreateSource("msmail", "owner@example.test")
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_id=?, source_is_from_me=TRUE WHERE id=?`), msmail.ID, id)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients SET email_address='owner@example.test' WHERE message_id=? AND recipient_type='from'`), id)
	requirements.NoError(err)

	got, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.test"}, nil)
	requirements.NoError(err)
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none", SeenInArchive: true}, got["target@example.test"],
		"legacy address-derived source ownership must not turn an incoming Microsoft message into an owner send")
}

func FuzzSenderRelationshipsMicrosoftSentEvidenceSurvivesSameSourceDedup(f *testing.F) {
	for _, seed := range [][5]bool{
		{false, false, false, false, false},
		{false, true, false, false, false},
		{true, false, false, false, false},
		{true, true, false, false, false},
		{true, false, true, false, false},
		{true, false, true, true, false},
		{true, false, false, false, true},
	} {
		f.Add(seed[0], seed[1], seed[2], seed[3], seed[4])
	}
	f.Fuzz(func(t *testing.T, duplicateSent, survivorSent, survivorDraft, survivorUnsent, duplicateBcc bool) {
		got := mergeMicrosoftSentRelationshipEvidenceCase(t, duplicateSent, survivorSent, survivorDraft, survivorUnsent, duplicateBcc)
		want := "none"
		if (duplicateSent || survivorSent) && !survivorUnsent {
			want = "known"
		}
		assert.Equal(t, want, got["target@example.test"].Relationship)
		if duplicateBcc {
			wantBcc := "none"
			if duplicateSent && !survivorUnsent {
				wantBcc = "known"
			}
			assert.Equal(t, wantBcc, got["bcc@example.test"].Relationship)
		}
	})
}

func TestSenderRelationshipsMicrosoftSentEvidenceOverridesRetainedDraft(t *testing.T) {
	got := mergeMicrosoftSentRelationshipEvidenceCase(t, true, false, true, false, false)
	assert.Equal(t, "known", got["target@example.test"].Relationship,
		"persisted provider Sent Items evidence must override a Drafts label retained from the survivor")
}

func TestSenderRelationshipsMicrosoftSentEvidenceDoesNotOverrideExplicitUnsent(t *testing.T) {
	got := mergeMicrosoftSentRelationshipEvidenceCase(t, true, false, true, true, false)
	assert.Equal(t, "none", got["target@example.test"].Relationship,
		"explicit is_sent=false must remain ineligible even when a duplicate has provider Sent Items evidence")
}

func TestSenderRelationshipsMicrosoftSentBccSurvivesSameSourceDedup(t *testing.T) {
	got := mergeMicrosoftSentRelationshipEvidenceCase(t, true, false, false, false, true)
	assert.Equal(t, "known", got["bcc@example.test"].Relationship,
		"sent-copy Bcc snapshots must survive when the archive copy is the deduplication survivor")
}

func mergeMicrosoftSentRelationshipEvidenceCase(t *testing.T, duplicateSent, survivorSent, survivorDraft, survivorUnsent, duplicateBcc bool) map[string]store.SenderRelationshipEvidence {
	t.Helper()
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("msmail", "owner@example.test")
	requirements.NoError(err)
	conversationID, err := f.Store.EnsureConversation(source.ID, "sent-thread", "Sent Thread")
	requirements.NoError(err)
	ownerID := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	targetID := f.EnsureParticipant("target@example.test", "Target", "example.test")
	bccID := int64(0)
	if duplicateBcc {
		bccID = f.EnsureParticipant("bcc@example.test", "Bcc", "example.test")
	}
	message := func(sourceMessageID string, withBcc, isUnsent bool) int64 {
		record := storetest.NewMessage(source.ID, conversationID).WithSourceMessageID(sourceMessageID).Build()
		record.RFC822MessageID = sql.NullString{String: "shared@example.test", Valid: true}
		record.SenderID = sql.NullInt64{Int64: ownerID, Valid: true}
		record.SentAt = sql.NullTime{Time: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Valid: true}
		record.IsSent = sql.NullBool{Bool: !isUnsent, Valid: true}
		recipients := []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{"owner@example.test"}},
			{Type: "to", ParticipantIDs: []int64{targetID}, EmailAddresses: []string{"target@example.test"}},
		}
		if withBcc {
			recipients = append(recipients, store.RecipientSet{Type: "bcc", ParticipantIDs: []int64{bccID}, EmailAddresses: []string{"bcc@example.test"}})
		}
		id, persistErr := f.Store.PersistMessage(&store.MessagePersistData{
			Message:    record,
			Recipients: recipients,
		})
		requirements.NoError(persistErr)
		return id
	}
	archiveDuplicate := message("archive-duplicate", duplicateBcc, false)
	archiveSurvivor := message("archive-survivor", false, survivorUnsent)
	folders, err := f.Store.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{
		"sent":    {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		"archive": {Name: "Archive", Type: "system"},
		"drafts":  {Name: "Drafts", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
	})
	requirements.NoError(err)
	sentFolder := folders["sent"]
	archiveFolder := folders["archive"]
	setHistory := func(messageID int64, hadSentEvidence bool) {
		if hadSentEvidence {
			_, reconcileErr := f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), messageID, &sentFolder, nil)
			requirements.NoError(reconcileErr)
		}
		_, reconcileErr := f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), messageID, &archiveFolder, nil)
		requirements.NoError(reconcileErr)
	}
	setHistory(archiveDuplicate, duplicateSent)
	setHistory(archiveSurvivor, survivorSent)
	if survivorDraft {
		draftFolder := folders["drafts"]
		_, err = f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), archiveSurvivor, &draftFolder, nil)
		requirements.NoError(err)
	}

	wantBeforeMerge := "none"
	if duplicateSent || (survivorSent && !survivorUnsent) {
		wantBeforeMerge = "known"
	}
	addresses := []string{"target@example.test"}
	if duplicateBcc {
		addresses = append(addresses, "bcc@example.test")
	}
	got, err := f.Store.SenderRelationships(t.Context(), addresses, nil)
	requirements.NoError(err)
	assertions.Equal(wantBeforeMerge, got["target@example.test"].Relationship)
	_, err = f.Store.MergeDuplicates(archiveSurvivor, []int64{archiveDuplicate}, "msmail-sent-dedup")
	requirements.NoError(err)

	got, err = f.Store.SenderRelationships(t.Context(), addresses, nil)
	requirements.NoError(err)
	return got
}

func TestSenderRelationshipsExcludeUnsentListBroadcastDeletedAndAddressOwnership(t *testing.T) {
	for _, kind := range []string{"unsent", "list", "broadcast", "deleted", "remote-deleted", "identity-derived"} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			address := "target@example.com"
			recipients := []string{address}
			if kind == "broadcast" {
				for i := range 30 {
					recipients = append(recipients, fmt.Sprintf("audience%d@example.com", i))
				}
			}
			id := senderRelationshipMessage(t, f, true, "to", recipients...)
			query := map[string]string{
				"unsent":           "UPDATE messages SET is_sent=FALSE WHERE id=?",
				"list":             "UPDATE messages SET list_id='news.example.com' WHERE id=?",
				"broadcast":        "UPDATE messages SET snippet='broadcast' WHERE id=?",
				"deleted":          "UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?",
				"remote-deleted":   "UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?",
				"identity-derived": "UPDATE messages SET source_is_from_me=FALSE, identity_is_from_me=TRUE, is_from_me=TRUE WHERE id=?",
			}[kind]
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(query), id)
			requirements.NoError(err)
			if kind == "unsent" {
				participant := f.EnsureParticipant(address, "Target", "example.com")
				_, _, err = f.Store.CreatePersonFromParticipant(participant)
				requirements.NoError(err)
			}
			got, err := f.Store.SenderRelationships(t.Context(), []string{address}, nil)
			requirements.NoError(err)
			assertions.Equal("none", got[address].Relationship)
			if kind == "unsent" {
				assertions.False(got[address].SeenInArchive, "draft cannot bootstrap provenance")
			}
		})
	}
}

func TestSenderRelationshipsScopeAndMergedEnvelope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	senderRelationshipMessage(t, f, true, "to", "chosen@example.com")
	survivor := f.EnsureParticipant("other@example.com", "Other", "example.com")
	absorbed := f.EnsureParticipant("chosen@example.com", "Chosen", "example.com")
	requirements.NoError(f.Store.MergeParticipants(absorbed, survivor))
	got, err := f.Store.SenderRelationships(t.Context(), []string{"chosen@example.com", "other@example.com"}, nil)
	requirements.NoError(err)
	assertions.Equal("known", got["chosen@example.com"].Relationship)
	assertions.Equal("none", got["other@example.com"].Relationship, "a send to an alias cannot trust the survivor's address")
	_, _, err = f.Store.CreatePersonFromParticipant(survivor)
	requirements.NoError(err)
	got, err = f.Store.SenderRelationships(t.Context(), []string{"chosen@example.com"}, nil)
	requirements.NoError(err)
	assertions.Equal("known", got["chosen@example.com"].Relationship, "alias does not inherit curated status")
	otherSource, err := f.Store.GetOrCreateSource("gmail", "second@example.com")
	requirements.NoError(err)
	for _, scope := range [][]int64{{}, {otherSource.ID}} {
		got, err = f.Store.SenderRelationships(t.Context(), []string{"chosen@example.com", "other@example.com"}, scope)
		requirements.NoError(err)
		for _, evidence := range got {
			assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none"}, evidence)
		}
	}
	got, err = f.Store.SenderRelationships(t.Context(), []string{"chosen@example.com"}, []int64{f.Source.ID})
	requirements.NoError(err)
	assertions.Equal("known", got["chosen@example.com"].Relationship)
}

func TestSenderRelationshipsMissingEnvelopeCannotTrustMergedAddress(t *testing.T) {
	for _, snapshot := range []any{nil, ""} {
		t.Run(fmt.Sprintf("snapshot-%v", snapshot), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			id := senderRelationshipMessage(t, f, true, "to", "chosen@example.com")
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients SET email_address=? WHERE message_id=? AND recipient_type='to'`), snapshot, id)
			requirements.NoError(err)
			survivor := f.EnsureParticipant("other@example.com", "Other", "example.com")
			absorbed := f.EnsureParticipant("chosen@example.com", "Chosen", "example.com")
			requirements.NoError(f.Store.MergeParticipants(absorbed, survivor))
			got, err := f.Store.SenderRelationships(t.Context(), []string{"other@example.com"}, nil)
			requirements.NoError(err)
			assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none"}, got["other@example.com"])
			_, _, err = f.Store.CreatePersonFromParticipant(survivor)
			requirements.NoError(err)
			got, err = f.Store.SenderRelationships(t.Context(), []string{"other@example.com"}, []int64{f.Source.ID})
			requirements.NoError(err)
			assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none"}, got["other@example.com"], "a missing snapshot cannot establish scoped contact reachability")
		})
	}
}

func TestSenderRelationshipsLimitsAndCancellation(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.Store.SenderRelationships(ctx, []string{"target@example.com"}, nil)
	requirements.ErrorIs(err, context.Canceled)
	_, err = f.Store.SenderRelationships(t.Context(), make([]string, store.MaxSenderRelationshipAddresses+1), nil)
	requirements.ErrorIs(err, store.ErrSenderRelationshipLimit)
	_, err = f.Store.SenderRelationships(t.Context(), []string{"a\x00@example.com"}, nil)
	requirements.Error(err)
	_, err = f.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, []int64{-1})
	requirements.Error(err)
	_, err = f.Store.SenderRelationships(t.Context(), []string{strings.Repeat("x", 513)}, nil)
	requirements.Error(err)
}

func TestSenderRelationshipsContactReachabilityAndDraftExclusion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	id := senderRelationshipMessage(t, f, true, "to", "target@example.com")
	participant := f.EnsureParticipant("target@example.com", "Target", "example.com")
	_, _, err := f.Store.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	requirements.NoError(err)
	for _, scope := range [][]int64{nil, {}, {f.Source.ID}, {other.ID}, {f.Source.ID, other.ID}} {
		got, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, scope)
		requirements.NoError(err)
		allowed := scope == nil
		for _, source := range scope {
			allowed = allowed || source == f.Source.ID
		}
		if allowed {
			assertions.Equal("contact", got["target@example.com"].Relationship)
			assertions.True(got["target@example.com"].SeenInArchive)
		} else {
			assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none"}, got["target@example.com"])
		}
	}
	// A source-native sent copy cannot make its own draft receipt safe.
	got, err := f.Store.SenderRelationshipsWithOptions(t.Context(), []string{"target@example.com"}, store.SenderRelationshipOptions{AllowedSourceIDs: []int64{f.Source.ID}, ExcludeMessageID: id})
	requirements.NoError(err)
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none"}, got["target@example.com"])
	// The owner can still use a curated contact with no prior archive history.
	// The excluded message must not invalidate that empty-history fallback.
	got, err = f.Store.SenderRelationshipsWithOptions(t.Context(), []string{"target@example.com"}, store.SenderRelationshipOptions{ExcludeMessageID: id})
	requirements.NoError(err)
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "contact"}, got["target@example.com"])
}

func TestSenderRelationshipsDraftOnlyContactWithoutSnapshot(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	id := senderRelationshipMessage(t, f, true, "to", "target@example.com")
	participant := f.EnsureParticipant("target@example.com", "Target", "example.com")
	_, _, err := f.Store.CreatePersonFromParticipant(participant)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), id)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_recipients SET email_address=NULL WHERE message_id=? AND recipient_type='to'`), id)
	requirements.NoError(err)
	got, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
	requirements.NoError(err)
	assert.Equal(t, store.SenderRelationshipEvidence{Relationship: "none"}, got["target@example.com"], "missing draft snapshots cannot trigger the saved-contact empty-history fallback")
}

func TestSenderRelationshipsChatRequiresNativeOneToOne(t *testing.T) {
	for _, members := range []int{2, 3} {
		t.Run(strconv.Itoa(members), func(t *testing.T) {
			requirements := require.New(t)
			f := storetest.New(t)
			owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
			target := f.EnsureParticipant("target@example.com", "Target", "example.com")
			conv, err := f.Store.EnsureConversationWithType(f.Source.ID, "direct", "chat", "Chat")
			requirements.NoError(err)
			requirements.NoError(f.Store.EnsureConversationParticipant(conv, owner, "member"))
			requirements.NoError(f.Store.EnsureConversationParticipant(conv, target, "member"))
			if members > 2 {
				third := f.EnsureParticipant("third@example.com", "Third", "example.com")
				requirements.NoError(f.Store.EnsureConversationParticipant(conv, third, "member"))
			}
			message := f.NewMessage().WithSentAt(time.Now()).WithIsFromMe(true).Build()
			message.SourceMessageID = "eml-native-chat-id"
			message.ConversationID = conv
			message.MessageType = "whatsapp"
			message.SenderID = sql.NullInt64{Int64: owner, Valid: true}
			_, err = f.Store.PersistMessage(&store.MessagePersistData{Message: message, Recipients: []store.RecipientSet{
				{Type: "to", ParticipantIDs: []int64{target}, EmailAddresses: []string{"target@example.com"}},
			}})
			requirements.NoError(err)
			got, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
			requirements.NoError(err)
			want := "none"
			if members == 2 {
				want = "known"
			}
			assert.Equal(t, want, got["target@example.com"].Relationship)
		})
	}
}

func TestSenderRelationshipsHistoryBudgetIsExplicit(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	target := f.EnsureParticipant("target@example.com", "Target", "example.com")
	// Populate metadata directly to exercise the real reader's history cap
	// without thousands of unrelated importer and projection transactions.
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<2049)
		INSERT INTO messages (source_id,conversation_id,source_message_id,message_type,source_is_from_me,is_from_me,is_sent,sent_at)
		SELECT ?,?, 'budget-' || CAST(x AS TEXT),'email',TRUE,TRUE,TRUE,NULL FROM n`), f.Source.ID, f.ConvID)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address)
		SELECT id,?,'to','target@example.com' FROM messages WHERE source_id=? AND source_message_id LIKE 'budget-%'`), target, f.Source.ID)
	requirements.NoError(err)
	got, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
	requirements.ErrorIs(err, store.ErrSenderRelationshipLimit)
	assertions.Nil(got, "partial evidence must not escape as a definitive result")
}

func TestSenderRelationshipsIncomingFromHistoryConsumesBudget(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	target := f.EnsureParticipant("target@example.com", "Target", "example.com")
	// An older owner send must not be treated as a complete answer when newer
	// incoming From rows already exhaust the historical-envelope scan budget.
	senderRelationshipMessage(t, f, true, "to", "target@example.com")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
		WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<2049)
		INSERT INTO messages (source_id,conversation_id,source_message_id,message_type,source_is_from_me,is_from_me,is_sent,sent_at)
		SELECT ?,?, 'incoming-from-budget-' || CAST(x AS TEXT),'email',FALSE,FALSE,TRUE,NULL FROM n`), f.Source.ID, f.ConvID)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address)
		SELECT id,?,'from','target@example.com' FROM messages WHERE source_id=? AND source_message_id LIKE 'incoming-from-budget-%'`), target, f.Source.ID)
	requirements.NoError(err)

	got, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
	requirements.ErrorIs(err, store.ErrSenderRelationshipLimit)
	assertions.Nil(got, "partial history must not escape as a definitive relationship")
}

func TestSenderRelationshipsContactHistoryBudgetPrecedesEligibilityAndScope(t *testing.T) {
	for _, kind := range []string{"deleted-only", "draft-only", "absent-in-scope"} {
		t.Run(kind, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := storetest.New(t)
			target := f.EnsureParticipant("target@example.test", "Target", "example.test")
			sourceID := f.Source.ID
			conversationID := f.ConvID
			var allowedSources []int64
			if kind == "absent-in-scope" {
				other, err := f.Store.GetOrCreateSource("gmail", "other@example.test")
				requirements.NoError(err)
				conversationID, err = f.Store.EnsureConversation(other.ID, "other-thread", "Other Thread")
				requirements.NoError(err)
				sourceID = other.ID
				allowedSources = []int64{f.Source.ID}
			}

			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
				WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<2049)
				INSERT INTO messages (source_id,conversation_id,source_message_id,message_type,source_is_from_me,is_from_me,is_sent,sent_at)
				SELECT ?,?, 'contact-budget-' || CAST(x AS TEXT),'email',TRUE,TRUE,TRUE,NULL FROM n`), sourceID, conversationID)
			requirements.NoError(err)
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address)
				SELECT id,?,'to','target@example.test' FROM messages WHERE source_id=? AND source_message_id LIKE 'contact-budget-%'`), target, sourceID)
			requirements.NoError(err)

			if kind == "deleted-only" {
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE source_id=? AND source_message_id LIKE 'contact-budget-%'`), sourceID)
				requirements.NoError(err)
			}
			if kind == "draft-only" {
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE source_id=? AND source_message_id LIKE 'contact-budget-%'`), sourceID)
				requirements.NoError(err)
			}

			got, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.test"}, allowedSources)
			requirements.ErrorIs(err, store.ErrSenderRelationshipLimit)
			assertions.Nil(got, "partial evidence must not escape as a definitive result")
		})
	}
}

func TestSenderRelationshipsMissingFromAndChatSnapshotsCannotFollowMerge(t *testing.T) {
	for _, kind := range []string{"from", "chat"} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			old := f.EnsureParticipant("old@example.com", "Old", "example.com")
			other := f.EnsureParticipant("other@example.com", "Other", "example.com")
			if kind == "from" {
				id := senderRelationshipMessage(t, f, false, "to", "old@example.com")
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM message_recipients WHERE message_id=?`), id)
				requirements.NoError(err)
			} else {
				owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
				conv, err := f.Store.EnsureConversationWithType(f.Source.ID, "direct", "chat", "Chat")
				requirements.NoError(err)
				requirements.NoError(f.Store.EnsureConversationParticipant(conv, owner, "member"))
				requirements.NoError(f.Store.EnsureConversationParticipant(conv, old, "member"))
				message := f.NewMessage().WithSentAt(time.Now()).WithIsFromMe(true).Build()
				message.ConversationID = conv
				message.MessageType = "whatsapp"
				message.SenderID = sql.NullInt64{Int64: owner, Valid: true}
				_, err = f.Store.UpsertMessage(message)
				requirements.NoError(err)
			}
			requirements.NoError(f.Store.MergeParticipants(old, other))
			got, err := f.Store.SenderRelationships(t.Context(), []string{"other@example.com"}, nil)
			requirements.NoError(err)
			assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none"}, got["other@example.com"])
			_, _, err = f.Store.CreatePersonFromParticipant(other)
			requirements.NoError(err)
			got, err = f.Store.SenderRelationships(t.Context(), []string{"other@example.com"}, []int64{f.Source.ID})
			requirements.NoError(err)
			assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none"}, got["other@example.com"])
		})
	}
}
