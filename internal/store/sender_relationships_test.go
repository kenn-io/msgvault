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

func TestSenderRelationshipsMicrosoftSentEvidenceUsesOnlySentEnvelope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("msmail", "owner@example.test")
	requirements.NoError(err)
	conversationID, err := f.Store.EnsureConversation(source.ID, "sent-envelope", "Sent Envelope")
	requirements.NoError(err)
	ownerID := f.EnsureParticipant("owner@example.test", "Owner", "example.test")

	message := func(sourceMessageID, recipientAddress string) int64 {
		recipientID := f.EnsureParticipant(recipientAddress, "Recipient", "example.test")
		record := storetest.NewMessage(source.ID, conversationID).WithSourceMessageID(sourceMessageID).Build()
		record.RFC822MessageID = sql.NullString{String: "shared-envelope@example.test", Valid: true}
		record.SenderID = sql.NullInt64{Int64: ownerID, Valid: true}
		record.SentAt = sql.NullTime{Time: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Valid: true}
		record.IsSent = sql.NullBool{Bool: true, Valid: true}
		id, persistErr := f.Store.PersistMessage(&store.MessagePersistData{
			Message: record,
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{"owner@example.test"}},
				{Type: "to", ParticipantIDs: []int64{recipientID}, EmailAddresses: []string{recipientAddress}},
			},
		})
		requirements.NoError(persistErr)
		return id
	}
	sentCopy := message("sent-copy", "sent@example.test")
	draftCopy := message("draft-copy", "draft-only@example.test")
	folders, err := f.Store.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{
		"sent":    {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		"archive": {Name: "Archive", Type: "system"},
		"drafts":  {Name: "Drafts", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
	})
	requirements.NoError(err)
	sentLabel, archiveLabel, draftsLabel := folders["sent"], folders["archive"], folders["drafts"]
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), sentCopy, &sentLabel, nil)
	requirements.NoError(err)
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), sentCopy, &archiveLabel, nil)
	requirements.NoError(err)
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), draftCopy, &draftsLabel, nil)
	requirements.NoError(err)
	_, err = f.Store.MergeDuplicates(draftCopy, []int64{sentCopy}, "msmail-sent-envelope")
	requirements.NoError(err)

	got, err := f.Store.SenderRelationships(t.Context(), []string{"sent@example.test", "draft-only@example.test"}, nil)
	requirements.NoError(err)
	assertions.Equal("known", got["sent@example.test"].Relationship)
	assertions.Equal("none", got["draft-only@example.test"].Relationship,
		"a retained draft envelope must not inherit a duplicate's Sent Items ownership")
}

func TestSenderRelationshipsUsesSavedSentAudienceAfterRecipientRefresh(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(
		`UPDATE sources SET source_type='msmail' WHERE id=?`), f.Source.ID)
	requirements.NoError(err)

	recipients := []string{"target@example.test"}
	for i := range 30 {
		recipients = append(recipients, fmt.Sprintf("audience%d@example.test", i))
	}
	messageID := senderRelationshipMessage(t, f, true, "to", recipients...)
	folders, err := f.Store.EnsureMicrosoftMailFoldersContext(ctx, f.Source.ID, map[string]store.LabelInfo{
		"sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	requirements.NoError(err)
	sentLabel := folders["sent"]
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(ctx, messageID, &sentLabel, nil)
	requirements.NoError(err)
	draftRecipient := f.EnsureParticipant("draft-only@example.test", "Draft Only", "example.test")
	requirements.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", []int64{draftRecipient}, []string{"Draft Only"}))
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(ctx, messageID, &sentLabel, nil)
	requirements.NoError(err)
	var draftOnlySaved bool
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`
		SELECT EXISTS(SELECT 1 FROM msmail_sent_envelope_recipients WHERE message_id=? AND email_address=?)
	`), messageID, "draft-only@example.test").Scan(&draftOnlySaved))
	assertions.False(draftOnlySaved, "a same-message MIME refresh must not enlarge the existing Sent snapshot")
	candidates, err := f.Store.LoadActivityCandidatesByIDContext(ctx, []int64{messageID})
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.False(store.ClassifyActivityCandidate(candidates[0], store.DefaultMaxDirectActivityCounterparts).Broadcast,
		"the refreshed MIME rows alone describe a direct message")

	got, err := f.Store.SenderRelationships(ctx, []string{"target@example.test"}, nil)
	requirements.NoError(err)
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none", SeenInArchive: true}, got["target@example.test"],
		"a refreshed MIME recipient list must not narrow the saved Sent broadcast audience")
}

func TestSenderRelationshipsUsesSavedSentAudienceForBroadcastDraftSurvivor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)

	draftRecipients := []string{"draft-only@example.test"}
	for i := range 30 {
		draftRecipients = append(draftRecipients, fmt.Sprintf("draft-%02d@example.test", i))
	}
	survivorID, sourceID := senderRelationshipMessageForSource(t, f, "gmail", "to", draftRecipients...)
	addSenderRelationshipSystemLabel(t, f, sourceID, "gmail", store.LabelSystemRoleDrafts, survivorID)

	sentCopyID, sentSourceID := senderRelationshipMessageForSource(t, f, "gmail", "to", "sent-recipient@example.test")
	addSenderRelationshipSystemLabel(t, f, sentSourceID, "gmail", store.LabelSystemRoleSent, sentCopyID)
	addSenderSentEnvelopeSnapshot(t, f, sentCopyID, false, "sent-recipient@example.test")
	_, err := f.Store.MergeDuplicates(survivorID, []int64{sentCopyID}, "saved-sent-broadcast-draft")
	requirements.NoError(err)

	candidates, err := f.Store.LoadActivityCandidatesByIDContext(t.Context(), []int64{survivorID})
	requirements.NoError(err)
	requirements.Len(candidates, 1)
	assertions.True(store.ClassifyActivityCandidate(candidates[0], store.DefaultMaxDirectActivityCounterparts).Broadcast,
		"the retained draft recipient rows exceed the direct-message limit")

	addresses := append([]string{"sent-recipient@example.test"}, draftRecipients...)
	got, err := f.Store.SenderRelationships(t.Context(), addresses, nil)
	requirements.NoError(err)
	assertions.Equal("known", got["sent-recipient@example.test"].Relationship,
		"the saved direct Sent audience must take precedence over the retained draft audience")
	for _, address := range draftRecipients {
		assertions.Equal("none", got[address].Relationship,
			"draft-only recipients must remain outside the authoritative Sent snapshot")
	}
}

func TestSenderRelationshipsMicrosoftSentEvidenceSurvivesRawImportSurvivor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("msmail", "owner@example.test")
	requirements.NoError(err)
	conversationID, err := f.Store.EnsureConversation(source.ID, "raw-sent-merge", "Raw Sent Merge")
	requirements.NoError(err)
	ownerID := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	targetID := f.EnsureParticipant("target@example.test", "Target", "example.test")
	message := func(sourceMessageID string) int64 {
		record := storetest.NewMessage(source.ID, conversationID).WithSourceMessageID(sourceMessageID).Build()
		record.RFC822MessageID = sql.NullString{String: "shared-raw-sent@example.test", Valid: true}
		record.SenderID = sql.NullInt64{Int64: ownerID, Valid: true}
		record.SentAt = sql.NullTime{Time: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Valid: true}
		record.IsSent = sql.NullBool{Bool: true, Valid: true}
		id, persistErr := f.Store.PersistMessage(&store.MessagePersistData{
			Message: record,
			Recipients: []store.RecipientSet{
				{Type: "from", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{"owner@example.test"}},
				{Type: "to", ParticipantIDs: []int64{targetID}, EmailAddresses: []string{"target@example.test"}},
				{Type: "bcc", ParticipantIDs: []int64{f.EnsureParticipant("bcc@example.test", "Bcc", "example.test")}, EmailAddresses: []string{"bcc@example.test"}},
			},
		})
		requirements.NoError(persistErr)
		return id
	}
	rawSurvivor := message("eml-" + strings.Repeat("a", 64))
	sentDuplicate := message("graph-sent-copy")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_is_from_me=TRUE WHERE id=?`), rawSurvivor)
	requirements.NoError(err)
	folders, err := f.Store.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{
		"sent":    {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		"archive": {Name: "Archive", Type: "system"},
	})
	requirements.NoError(err)
	sentLabel, archiveLabel := folders["sent"], folders["archive"]
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), sentDuplicate, &sentLabel, nil)
	requirements.NoError(err)
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), sentDuplicate, &archiveLabel, nil)
	requirements.NoError(err)
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), rawSurvivor, &archiveLabel, nil)
	requirements.NoError(err)
	_, err = f.Store.MergeDuplicates(rawSurvivor, []int64{sentDuplicate}, "msmail-raw-sent-merge")
	requirements.NoError(err)

	got, err := f.Store.SenderRelationships(t.Context(), []string{"bcc@example.test"}, nil)
	requirements.NoError(err)
	assertions.Equal("known", got["bcc@example.test"].Relationship,
		"provider-observed Sent provenance must survive a raw-import message ID")
}

func TestSenderRelationshipsMicrosoftSentLabelWithoutMetadataPreservesBcc(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("msmail", "owner@example.test")
	requirements.NoError(err)
	conversationID, err := f.Store.EnsureConversation(source.ID, "label-sent-merge", "Label Sent Merge")
	requirements.NoError(err)
	ownerID := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	targetID := f.EnsureParticipant("target@example.test", "Target", "example.test")
	bccID := f.EnsureParticipant("bcc@example.test", "Bcc", "example.test")
	message := func(sourceMessageID string, withBcc bool) int64 {
		record := storetest.NewMessage(source.ID, conversationID).WithSourceMessageID(sourceMessageID).Build()
		record.RFC822MessageID = sql.NullString{String: "shared-label-sent@example.test", Valid: true}
		record.SenderID = sql.NullInt64{Int64: ownerID, Valid: true}
		record.SentAt = sql.NullTime{Time: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Valid: true}
		record.IsSent = sql.NullBool{Bool: true, Valid: true}
		recipients := []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{"owner@example.test"}},
			{Type: "to", ParticipantIDs: []int64{targetID}, EmailAddresses: []string{"target@example.test"}},
		}
		if withBcc {
			recipients = append(recipients, store.RecipientSet{Type: "bcc", ParticipantIDs: []int64{bccID}, EmailAddresses: []string{"bcc@example.test"}})
		}
		id, persistErr := f.Store.PersistMessage(&store.MessagePersistData{Message: record, Recipients: recipients})
		requirements.NoError(persistErr)
		return id
	}
	survivor := message("archive-copy", false)
	sentDuplicate := message("sent-copy", true)
	folders, err := f.Store.EnsureMicrosoftMailFoldersContext(t.Context(), source.ID, map[string]store.LabelInfo{
		"sent":    {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
		"archive": {Name: "Archive", Type: "system"},
	})
	requirements.NoError(err)
	sentLabel, archiveLabel := folders["sent"], folders["archive"]
	requirements.NoError(f.Store.AddMessageLabels(sentDuplicate, []int64{sentLabel}))
	requirements.NoError(f.Store.AddMessageLabels(survivor, []int64{archiveLabel}))
	var metadata sql.NullString
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT metadata FROM messages WHERE id=?`), sentDuplicate).Scan(&metadata))
	assertions.False(metadata.Valid, "the duplicate has provider Sent labeling but no persisted metadata marker")
	_, err = f.Store.MergeDuplicates(survivor, []int64{sentDuplicate}, "msmail-label-sent-merge")
	requirements.NoError(err)

	got, err := f.Store.SenderRelationships(t.Context(), []string{"bcc@example.test"}, nil)
	requirements.NoError(err)
	assertions.Equal("known", got["bcc@example.test"].Relationship,
		"a same-source Sent label must preserve the hidden duplicate's Bcc evidence")
}

func TestSenderRelationshipsMicrosoftSentEvidenceTransfersAcrossSources(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	ownerID := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	bccID := f.EnsureParticipant("private@example.test", "Private Recipient", "example.test")
	history, err := f.Store.GetOrCreateSource("msmail", "history@example.test")
	requirements.NoError(err)
	live, err := f.Store.GetOrCreateSource("msmail", "live@example.test")
	requirements.NoError(err)

	message := func(sourceID int64, conversationKey, sourceMessageID string, withBcc bool) int64 {
		conversationID, err := f.Store.EnsureConversationWithType(sourceID, conversationKey, "email_thread", "Sent thread")
		requirements.NoError(err)
		record := storetest.NewMessage(sourceID, conversationID).WithSourceMessageID(sourceMessageID).Build()
		record.SenderID = sql.NullInt64{Int64: ownerID, Valid: true}
		record.SentAt = sql.NullTime{Time: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Valid: true}
		record.IsSent = sql.NullBool{Bool: true, Valid: true}
		record.IsFromMe = true
		recipients := []store.RecipientSet{{
			Type: "from", ParticipantIDs: []int64{ownerID}, EmailAddresses: []string{"owner@example.test"},
		}}
		if withBcc {
			recipients = append(recipients, store.RecipientSet{
				Type: "bcc", ParticipantIDs: []int64{bccID}, EmailAddresses: []string{"private@example.test"},
			})
		}
		id, err := f.Store.PersistMessage(&store.MessagePersistData{Message: record, Recipients: recipients})
		requirements.NoError(err)
		return id
	}
	survivor := message(live.ID, "live-sent-thread", "live-copy", false)
	duplicate := message(history.ID, "history-sent-thread", "history-copy", true)
	folders, err := f.Store.EnsureMicrosoftMailFoldersContext(ctx, history.ID, map[string]store.LabelInfo{
		"sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	requirements.NoError(err)
	sentLabel := folders["sent"]
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(ctx, duplicate, &sentLabel, nil)
	requirements.NoError(err)

	_, err = f.Store.MergeDuplicates(survivor, []int64{duplicate}, "msmail-cross-source-sent-merge")
	requirements.NoError(err)
	var metadata sql.NullString
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
		`SELECT metadata FROM messages WHERE id=?`), survivor).Scan(&metadata))
	assertions.True(metadata.Valid && strings.Contains(metadata.String, `"msmail_sent_items_observed":true`))
	var hasBccSnapshot bool
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`
		SELECT EXISTS(SELECT 1 FROM msmail_sent_envelope_recipients WHERE message_id=? AND email_address=?)
	`), survivor, "private@example.test").Scan(&hasBccSnapshot))
	assertions.True(hasBccSnapshot)
	got, err := f.Store.SenderRelationships(ctx, []string{"private@example.test"}, nil)
	requirements.NoError(err)
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "known", SeenInArchive: true}, got["private@example.test"])
}

func TestSenderRelationshipsSentSnapshotsKeepFromAndRejectLiveRecipientFallback(t *testing.T) {
	for _, sentRecipients := range [][]string{nil, {"sent-only@example.test"}} {
		t.Run(map[bool]string{true: "nonempty", false: "empty"}[len(sentRecipients) > 0], func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := storetest.New(t)
			ctx := t.Context()
			_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(
				`UPDATE sources SET source_type='msmail' WHERE id=?`), f.Source.ID)
			requirements.NoError(err)
			survivor := senderRelationshipMessage(t, f, true, "to", "draft-only@example.test")
			sentCopyRecipients := sentRecipients
			if len(sentCopyRecipients) == 0 {
				sentCopyRecipients = []string{"temporary@example.test"}
			}
			sentCopy := senderRelationshipMessage(t, f, true, "to", sentCopyRecipients...)
			folders, err := f.Store.EnsureMicrosoftMailFoldersContext(ctx, f.Source.ID, map[string]store.LabelInfo{
				"sent":   {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
				"drafts": {Name: "Drafts", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
			})
			requirements.NoError(err)
			sentLabel, draftsLabel := folders["sent"], folders["drafts"]
			if len(sentRecipients) == 0 {
				_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(
					`DELETE FROM message_recipients WHERE message_id=? AND recipient_type='to'`), sentCopy)
				requirements.NoError(err)
			}
			_, err = f.Store.ReconcileMicrosoftMailLabelsContext(ctx, sentCopy, &sentLabel, nil)
			requirements.NoError(err)
			_, err = f.Store.ReconcileMicrosoftMailLabelsContext(ctx, survivor, &draftsLabel, nil)
			requirements.NoError(err)
			_, err = f.Store.MergeDuplicates(survivor, []int64{sentCopy}, "msmail-snapshot-boundary")
			requirements.NoError(err)

			ownerID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
			_, _, err = f.Store.CreatePersonFromParticipant(ownerID)
			requirements.NoError(err)
			addresses := []string{"owner@example.com", "draft-only@example.test"}
			if len(sentRecipients) > 0 {
				addresses = append(addresses, sentRecipients[0])
			}
			got, err := f.Store.SenderRelationships(ctx, addresses, nil)
			requirements.NoError(err)
			assertions.Equal(store.SenderRelationshipEvidence{Relationship: "contact", SeenInArchive: true}, got["owner@example.com"],
				"From evidence remains available even when Sent recipients come from a saved snapshot")
			assertions.Equal(store.SenderRelationshipEvidence{Relationship: "none"}, got["draft-only@example.test"],
				"the authoritative Sent snapshot must block fallback to replaceable draft recipients")
			if len(sentRecipients) > 0 {
				assertions.Equal("known", got[sentRecipients[0]].Relationship)
			}
		})
	}
}

func TestSenderRelationshipsMicrosoftSentDuplicateMergesIntoExistingSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE sources SET source_type='msmail' WHERE id=?`), f.Source.ID)
	requirements.NoError(err)
	survivor := senderRelationshipMessage(t, f, true, "to", "draft-only@example.com")
	duplicate := senderRelationshipMessage(t, f, true, "bcc", "sent-bcc@example.com")
	folders, err := f.Store.EnsureMicrosoftMailFoldersContext(t.Context(), f.Source.ID, map[string]store.LabelInfo{
		"sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	requirements.NoError(err)
	sentLabel := folders["sent"]
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(t.Context(), duplicate, &sentLabel, nil)
	requirements.NoError(err)
	// A legacy Sent duplicate may have authoritative metadata and a live
	// envelope but no saved snapshot, while its survivor already has a snapshot.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`DELETE FROM msmail_sent_envelope_recipients WHERE message_id=?`), duplicate)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE messages SET metadata=? WHERE id=?`), `{"msmail_sent_items_observed":true}`, duplicate)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO msmail_sent_envelope_recipients(
		message_id, email_address) VALUES (?, ?)`), survivor, "previous-sent@example.com")
	requirements.NoError(err)

	_, err = f.Store.MergeDuplicates(survivor, []int64{duplicate}, "msmail-union-sent-envelope")
	requirements.NoError(err)
	got, err := f.Store.SenderRelationships(t.Context(), []string{
		"previous-sent@example.com", "sent-bcc@example.com", "draft-only@example.com",
	}, nil)
	requirements.NoError(err)
	assertions.Equal("known", got["previous-sent@example.com"].Relationship)
	assertions.Equal("known", got["sent-bcc@example.com"].Relationship)
	assertions.Equal("none", got["draft-only@example.com"].Relationship,
		"the saved Sent snapshot must not include the survivor's replaceable draft recipients")
}

func TestSenderRelationshipsGmailAndIMAPDuplicatePreserveSentEnvelope(t *testing.T) {
	for _, provider := range []string{"gmail", "imap"} {
		t.Run(provider, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
				`UPDATE sources SET source_type=? WHERE id=?`), provider, f.Source.ID)
			requirements.NoError(err)
			survivor := senderRelationshipMessage(t, f, true, "to", "draft-only@example.com")
			sentDuplicate := senderRelationshipMessage(t, f, true, "to", "sent-only@example.com")
			labelID := "Sent"
			if provider == "gmail" {
				labelID = "SENT"
			}
			labels, err := f.Store.EnsureLabelsBatch(f.Source.ID, map[string]store.LabelInfo{
				labelID: {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
			})
			requirements.NoError(err)
			requirements.NoError(f.Store.AddMessageLabels(sentDuplicate, []int64{labels[labelID]}))
			_, err = f.Store.MergeDuplicates(survivor, []int64{sentDuplicate}, "provider-sent-envelope-"+provider)
			requirements.NoError(err)

			got, err := f.Store.SenderRelationships(t.Context(), []string{
				"sent-only@example.com", "draft-only@example.com",
			}, nil)
			requirements.NoError(err)
			assertions.Equal("known", got["sent-only@example.com"].Relationship)
			assertions.Equal("none", got["draft-only@example.com"].Relationship,
				"a Sent label transferred from the duplicate must use that exact envelope")
		})
	}
}

func TestSenderRelationshipsProviderSentSnapshotSurvivesWithoutCurrentSentLabel(t *testing.T) {
	for _, provider := range []string{"gmail", "imap"} {
		t.Run(provider, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := storetest.New(t)
			ctx := t.Context()
			_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(
				`UPDATE sources SET source_type=? WHERE id=?`), provider, f.Source.ID)
			requirements.NoError(err)
			survivor := senderRelationshipMessage(t, f, true, "to", "draft-only@example.test")
			sentCopy := senderRelationshipMessage(t, f, true, "to", "sent-only@example.test")
			draftID := "Drafts"
			if provider == "gmail" {
				draftID = "DRAFT"
			}
			labels, err := f.Store.EnsureLabelsBatch(f.Source.ID, map[string]store.LabelInfo{
				draftID: {Name: "Drafts", Type: "system", SystemRole: store.LabelSystemRoleDrafts},
			})
			requirements.NoError(err)
			requirements.NoError(f.Store.AddMessageLabels(survivor, []int64{labels[draftID]}))
			_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
				INSERT INTO provider_sent_envelope_recipients(message_id, email_address) VALUES (?, ?)
			`), sentCopy, "sent-only@example.test")
			requirements.NoError(err)

			_, err = f.Store.MergeDuplicates(survivor, []int64{sentCopy}, "provider-sent-snapshot-without-label-"+provider)
			requirements.NoError(err)
			got, err := f.Store.SenderRelationships(ctx, []string{"sent-only@example.test", "draft-only@example.test"}, nil)
			requirements.NoError(err)
			assertions.Equal("known", got["sent-only@example.test"].Relationship)
			assertions.Equal("none", got["draft-only@example.test"].Relationship)

			candidates, err := f.Store.LoadActivityCandidatesByIDContext(ctx, []int64{survivor})
			requirements.NoError(err)
			requirements.Len(candidates, 1)
			assertions.False(candidates[0].Unsent,
				"a saved provider Sent envelope keeps the merged message sent after its Sent label disappears")
			_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), survivor)
			requirements.NoError(err)
			candidates, err = f.Store.LoadActivityCandidatesByIDContext(ctx, []int64{survivor})
			requirements.NoError(err)
			requirements.Len(candidates, 1)
			assertions.True(candidates[0].Unsent,
				"an explicit is_sent=false value takes precedence over transferred provider provenance")
		})
	}
}

func FuzzSenderSentEnvelopeMergesAcrossProviderCarriers(f *testing.F) {
	for _, seed := range []struct {
		scenario      uint8
		emptySnapshot bool
	}{
		{0, true},
		{1, false},
		{1, true},
		{2, false},
		{2, true},
		{3, false},
		{3, true},
	} {
		f.Add(seed.scenario, seed.emptySnapshot)
	}
	f.Fuzz(func(t *testing.T, scenario uint8, emptySnapshot bool) {
		if scenario > 3 {
			t.Skip()
		}
		t.Run(fmt.Sprintf("scenario-%d-empty-%t", scenario, emptySnapshot), func(t *testing.T) {
			runSenderSentEnvelopeCarrierScenario(t, scenario, emptySnapshot)
		})
	})
}

func runSenderSentEnvelopeCarrierScenario(t *testing.T, scenario uint8, emptySnapshot bool) {
	t.Helper()
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()

	var survivorID int64
	var survivorSourceID int64
	var addresses []string
	wantKnown := map[string]bool{}
	var duplicateID int64
	switch scenario {
	case 0:
		// An explicitly empty Microsoft snapshot must keep a Sent-labeled
		// survivor's replaceable live recipients out of the provider snapshot.
		survivorID, survivorSourceID = senderRelationshipMessageForSource(
			t, f, "msmail", "to", "draft-only@example.test",
		)
		duplicateID, _ = senderRelationshipMessageForSource(
			t, f, "gmail", "to", "duplicate-only@example.test",
		)
		addSenderRelationshipSystemLabel(t, f, survivorSourceID, "msmail", store.LabelSystemRoleSent, survivorID)
		addSenderSentEnvelopeSnapshot(t, f, survivorID, true)
		addresses = []string{"draft-only@example.test", "duplicate-only@example.test"}
	case 1:
		// Capture a native Gmail Sent envelope before an incoming Microsoft
		// snapshot makes the survivor's live recipient rows non-authoritative.
		role := "bcc"
		survivorID, survivorSourceID = senderRelationshipMessageForSource(
			t, f, "gmail", role, "gmail-only@example.test",
		)
		duplicateID, _ = senderRelationshipMessageForSource(
			t, f, "msmail", "to", "microsoft-live@example.test",
		)
		addSenderRelationshipSystemLabel(t, f, survivorSourceID, "gmail", store.LabelSystemRoleSent, survivorID)
		var microsoftRecipients []string
		if !emptySnapshot {
			microsoftRecipients = []string{"microsoft-only@example.test"}
		}
		addSenderSentEnvelopeSnapshot(t, f, duplicateID, true, microsoftRecipients...)
		addresses = []string{"gmail-only@example.test", "microsoft-only@example.test", "repeated-draft@example.test"}
		wantKnown["gmail-only@example.test"] = true
		wantKnown["microsoft-only@example.test"] = !emptySnapshot
	case 2:
		// An established Gmail snapshot can travel on a Microsoft carrier,
		// including an empty snapshot that still marks the survivor as sent.
		survivorID, survivorSourceID = senderRelationshipMessageForSource(
			t, f, "msmail", "to", "draft-only@example.test",
		)
		duplicateID, _ = senderRelationshipMessageForSource(
			t, f, "gmail", "to", "gmail-live@example.test",
		)
		addSenderRelationshipSystemLabel(t, f, survivorSourceID, "msmail", store.LabelSystemRoleDrafts, survivorID)
		var providerRecipients []string
		if !emptySnapshot {
			providerRecipients = []string{"provider-only@example.test"}
		}
		addSenderSentEnvelopeSnapshot(t, f, duplicateID, false, providerRecipients...)
		addresses = []string{"draft-only@example.test", "provider-only@example.test"}
		wantKnown["provider-only@example.test"] = !emptySnapshot
	case 3:
		// Microsoft Sent evidence also has to override draft classification
		// after it transfers to a Gmail draft survivor.
		survivorID, survivorSourceID = senderRelationshipMessageForSource(
			t, f, "gmail", "to", "draft-only@example.test",
		)
		duplicateID, _ = senderRelationshipMessageForSource(
			t, f, "msmail", "to", "microsoft-live@example.test",
		)
		addSenderRelationshipSystemLabel(t, f, survivorSourceID, "gmail", store.LabelSystemRoleDrafts, survivorID)
		var microsoftRecipients []string
		if !emptySnapshot {
			microsoftRecipients = []string{"microsoft-only@example.test"}
		}
		addSenderSentEnvelopeSnapshot(t, f, duplicateID, true, microsoftRecipients...)
		addresses = []string{"draft-only@example.test", "microsoft-only@example.test"}
		wantKnown["microsoft-only@example.test"] = !emptySnapshot
	}

	_, err := f.Store.MergeDuplicates(survivorID, []int64{duplicateID}, "sender-envelope-carrier")
	requirements.NoError(err)
	got, err := f.Store.SenderRelationships(ctx, addresses, nil)
	requirements.NoError(err)
	for _, address := range addresses {
		want := "none"
		if wantKnown[address] {
			want = "known"
		}
		assertions.Equal(want, got[address].Relationship, address)
	}

	if scenario == 1 {
		repeatedID, _ := senderRelationshipMessageForSource(
			t, f, "msmail", "to", "repeated-draft@example.test",
		)
		_, err := f.Store.MergeDuplicates(survivorID, []int64{repeatedID}, "sender-envelope-carrier-repeat")
		requirements.NoError(err)
		got, err = f.Store.SenderRelationships(ctx, addresses, nil)
		requirements.NoError(err)
		assertions.Equal("known", got["gmail-only@example.test"].Relationship,
			"the first merge must preserve survivor-only Bcc evidence across a later carrier merge")
		assertions.Equal("none", got["repeated-draft@example.test"].Relationship,
			"a later draft-only carrier must not contaminate either authoritative snapshot")
	}

	if scenario == 2 || scenario == 3 {
		candidates, err := f.Store.LoadActivityCandidatesByIDContext(ctx, []int64{survivorID})
		requirements.NoError(err)
		requirements.Len(candidates, 1)
		assertions.False(candidates[0].Unsent,
			"transferred Sent provenance overrides draft evidence on every supported provider")
		_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), survivorID)
		requirements.NoError(err)
		candidates, err = f.Store.LoadActivityCandidatesByIDContext(ctx, []int64{survivorID})
		requirements.NoError(err)
		requirements.Len(candidates, 1)
		assertions.True(candidates[0].Unsent,
			"an explicit is_sent=false value takes precedence over transferred provenance")
	}
}

func TestSenderRelationshipsSentSnapshotDoesNotAbsorbDraftRecipientsFromMerge(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()

	survivorID, survivorSourceID := senderRelationshipMessageForSource(
		t, f, "msmail", "to", "sent-recipient@example.test",
	)
	addSenderRelationshipSystemLabel(t, f, survivorSourceID, "msmail", store.LabelSystemRoleSent, survivorID)
	addSenderSentEnvelopeSnapshot(t, f, survivorID, true, "sent-recipient@example.test")

	draftRecipients := make([]string, 30)
	for i := range draftRecipients {
		draftRecipients[i] = fmt.Sprintf("draft-recipient-%02d@example.test", i)
	}
	duplicateID, duplicateSourceID := senderRelationshipMessageForSource(
		t, f, "gmail", "to", draftRecipients...,
	)
	addSenderRelationshipSystemLabel(t, f, duplicateSourceID, "gmail", store.LabelSystemRoleDrafts, duplicateID)
	_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), duplicateID)
	requirements.NoError(err)

	_, err = f.Store.MergeDuplicates(survivorID, []int64{duplicateID}, "sent-survivor-draft-duplicate")
	requirements.NoError(err)

	addresses := append([]string{"sent-recipient@example.test"}, draftRecipients...)
	got, err := f.Store.SenderRelationships(ctx, addresses, nil)
	requirements.NoError(err)
	assertions.Equal("known", got["sent-recipient@example.test"].Relationship,
		"non-Sent duplicate recipients must not turn a saved direct Sent message into a broadcast")
	for _, address := range draftRecipients {
		assertions.Equal("none", got[address].Relationship,
			"draft-only recipients must remain outside the authoritative Sent snapshot")
	}

	var liveRecipients int
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT COUNT(*)
		FROM message_recipients WHERE message_id=? AND recipient_type IN ('to','cc','bcc')`), survivorID).Scan(&liveRecipients))
	assertions.Equal(1, liveRecipients, "replaceable draft recipients must not be copied to the Sent survivor")
}

func senderRelationshipMessageForSource(
	t *testing.T, f *storetest.Fixture, sourceType, role string, recipients ...string,
) (int64, int64) {
	t.Helper()
	requirements := require.New(t)
	source, err := f.Store.GetOrCreateSource(sourceType, "owner@example.com")
	requirements.NoError(err)
	messageID := senderRelationshipMessage(t, f, true, role, recipients...)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE messages SET source_id=? WHERE id=?`), source.ID, messageID)
	requirements.NoError(err)
	return messageID, source.ID
}

func addSenderRelationshipSystemLabel(
	t *testing.T, f *storetest.Fixture, sourceID int64, sourceType, role string, messageID int64,
) {
	t.Helper()
	requirements := require.New(t)
	providerLabelID := "Sent"
	name := "Sent"
	if role == store.LabelSystemRoleDrafts {
		providerLabelID, name = "Drafts", "Drafts"
	}
	if sourceType == "gmail" {
		providerLabelID = "SENT"
		if role == store.LabelSystemRoleDrafts {
			providerLabelID = "DRAFT"
		}
	}
	var labelID int64
	if sourceType == "msmail" {
		providerLabelID = "sent"
		if role == store.LabelSystemRoleDrafts {
			providerLabelID = "drafts"
		}
		folders, err := f.Store.EnsureMicrosoftMailFoldersContext(t.Context(), sourceID,
			map[string]store.LabelInfo{providerLabelID: {Name: name, Type: "system", SystemRole: role}})
		requirements.NoError(err)
		labelID = folders[providerLabelID]
	} else {
		labels, err := f.Store.EnsureLabelsBatch(sourceID, map[string]store.LabelInfo{
			providerLabelID: {Name: name, Type: "system", SystemRole: role},
		})
		requirements.NoError(err)
		labelID = labels[providerLabelID]
	}
	requirements.NoError(f.Store.AddMessageLabels(messageID, []int64{labelID}))
}

func addSenderSentEnvelopeSnapshot(
	t *testing.T, f *storetest.Fixture, messageID int64, microsoft bool, addresses ...string,
) {
	t.Helper()
	requirements := require.New(t)
	metadata := `{"provider_sent_envelope_observed":true}`
	statement := `INSERT INTO provider_sent_envelope_recipients(message_id, email_address) VALUES (?, ?)`
	if microsoft {
		metadata = `{"msmail_sent_items_observed":true,"msmail_sent_envelope_observed":true}`
		statement = `INSERT INTO msmail_sent_envelope_recipients(message_id, email_address) VALUES (?, ?)`
	}
	requirements.NoError(f.Store.SetMessageMetadata(messageID, sql.NullString{String: metadata, Valid: true}))
	for _, address := range addresses {
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(statement), messageID, address)
		requirements.NoError(err)
	}
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

func TestSenderRelationshipsBoundsLiveRecipientsWhenSentSnapshotExists(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	messageID := senderRelationshipMessage(t, f, true, "to", "target@example.com")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE sources SET source_type='msmail' WHERE id=?`), f.Source.ID)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO msmail_sent_envelope_recipients(
		message_id, email_address
	) VALUES (?, ?)`), messageID, "target@example.com")
	requirements.NoError(err)

	_, err = f.Store.DB().ExecContext(t.Context(), `WITH RECURSIVE digit(x) AS (
		SELECT 0 UNION ALL SELECT x+1 FROM digit WHERE x<100
	)
	INSERT INTO participants(email_address, display_name, domain)
	SELECT 'bulk-' || CAST(a.x*101+b.x AS TEXT) || '@example.test', 'Bulk Recipient', 'example.test'
	FROM digit a CROSS JOIN digit b
	WHERE a.x*101+b.x < 10001`)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(
		message_id, participant_id, recipient_type, email_address
	)
	SELECT ?, id, 'to', email_address FROM participants
	WHERE email_address LIKE 'bulk-%@example.test'`), messageID)
	requirements.NoError(err)

	got, err := f.Store.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
	requirements.ErrorIs(err, store.ErrSenderRelationshipLimit)
	assertions.Nil(got, "snapshot-backed messages still account for live-recipient rows read by activity loading")
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

func TestSenderRelationshipsCountsSentSnapshotRowsBeforeDeduplicatingHistory(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	target := f.EnsureParticipant("target@example.test", "Target", "example.test")
	source, err := f.Store.GetOrCreateSource("msmail", "owner@example.test")
	requirements.NoError(err)
	conversationID, err := f.Store.EnsureConversationWithType(source.ID, "snapshot-budget", "email_thread", "Snapshot budget")
	requirements.NoError(err)
	const messageCount = 1025
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
		WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x < ?)
		INSERT INTO messages (source_id, conversation_id, source_message_id, message_type, source_is_from_me, is_from_me, is_sent)
		SELECT ?, ?, 'snapshot-budget-' || CAST(x AS TEXT), 'email', TRUE, TRUE, TRUE FROM n
	`), messageCount, source.ID, conversationID)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
		INSERT INTO message_recipients(message_id, participant_id, recipient_type, email_address)
		SELECT id, ?, 'to', 'target@example.test' FROM messages
		WHERE source_id=? AND source_message_id LIKE 'snapshot-budget-%'
	`), target, source.ID)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`
		INSERT INTO msmail_sent_envelope_recipients(message_id, email_address)
		SELECT id, 'target@example.test' FROM messages
		WHERE source_id=? AND source_message_id LIKE 'snapshot-budget-%'
	`), source.ID)
	requirements.NoError(err)

	got, err := f.Store.SenderRelationships(ctx, []string{"target@example.test"}, nil)
	requirements.ErrorIs(err, store.ErrSenderRelationshipLimit)
	assertions.Nil(got, "duplicate envelope rows must still consume the bounded history budget")
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
