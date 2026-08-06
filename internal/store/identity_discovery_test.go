package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type identityDiscoveryFixture struct {
	Store       *store.Store
	SourceID    int64
	SentID      int64
	InboundID   int64
	GmailSentID int64
	When        time.Time
}

func newIdentityDiscoveryFixture(t *testing.T) identityDiscoveryFixture {
	t.Helper()
	f := storetest.New(t)
	when := time.Date(2026, 7, 31, 12, 30, 0, 0, time.UTC)

	createMessage := func(sourceMessageID string, sentAt, receivedAt sql.NullTime, isFromMe bool) int64 {
		t.Helper()
		id, err := f.Store.UpsertMessage(&store.Message{
			ConversationID:  f.ConvID,
			SourceID:        f.Source.ID,
			SourceMessageID: sourceMessageID,
			MessageType:     "email",
			SentAt:          sentAt,
			ReceivedAt:      receivedAt,
			IsFromMe:        isFromMe,
			SizeEstimate:    100,
		})
		require.NoError(t, err, "create discovery message")
		return id
	}

	sentID := createMessage("sent-folder-message", sql.NullTime{Time: when, Valid: true}, sql.NullTime{}, false)
	inboundID := createMessage("inbound-message", sql.NullTime{}, sql.NullTime{Time: when.Add(time.Hour), Valid: true}, false)
	gmailSentID := createMessage("gmail-sent-message", sql.NullTime{}, sql.NullTime{}, true)
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET archived_at = ? WHERE id = ?`), when.Add(2*time.Hour), gmailSentID)
	require.NoError(t, err, "fix archived-at fallback")

	sentLabelID, err := f.Store.EnsureLabel(f.Source.ID, "imap-sent", "Sent Mail", "system")
	require.NoError(t, err, "create canonical sent folder")
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE labels SET system_role = 'sent' WHERE id = ?`), sentLabelID)
	require.NoError(t, err, "set canonical sent role")
	require.NoError(t, f.Store.ReplaceMessageLabels(sentID, []int64{sentLabelID}), "label sent message")

	gmailSentLabelID, err := f.Store.EnsureLabel(f.Source.ID, "SENT", "Gmail Sent Mail", "system")
	require.NoError(t, err, "create Gmail sent label")
	require.NoError(t, f.Store.ReplaceMessageLabels(gmailSentID, []int64{gmailSentLabelID}), "label Gmail sent message")

	nameOnlySentID, err := f.Store.EnsureLabel(f.Source.ID, "mailbox-42", "Sent", "user")
	require.NoError(t, err, "create name-only sent label")
	require.NoError(t, f.Store.ReplaceMessageLabels(inboundID, []int64{nameOnlySentID}), "label inbound message")

	addRecipients := func(messageID int64, recipientType string, addresses ...string) {
		t.Helper()
		participantIDs := make([]int64, 0, len(addresses))
		displayNames := make([]string, 0, len(addresses))
		for _, address := range addresses {
			participantID, participantErr := f.Store.EnsureParticipant(address, "Test User", "example.test")
			require.NoError(t, participantErr, "create discovery participant")
			participantIDs = append(participantIDs, participantID)
			displayNames = append(displayNames, "Test User")
		}
		require.NoError(t,
			f.Store.ReplaceMessageRecipients(messageID, recipientType, participantIDs, displayNames),
			"add discovery recipients")
	}

	addRecipients(sentID, "from", "Masked-Shop@Example.test", "second-from@example.test")
	addRecipients(sentID, "to", "sent-to@example.test")
	addRecipients(sentID, "cc", "sent-cc@example.test")
	addRecipients(sentID, "bcc", "sent-bcc@example.test")
	addRecipients(inboundID, "to", "inbound-only@example.test")
	addRecipients(gmailSentID, "from", "gmail-sender@example.test")

	return identityDiscoveryFixture{
		Store:       f.Store,
		SourceID:    f.Source.ID,
		SentID:      sentID,
		InboundID:   inboundID,
		GmailSentID: gmailSentID,
		When:        when,
	}
}

func TestIdentityDiscoveryPageReadsStrongWeakAndAllRecipientMetadata(t *testing.T) {
	assert := assert.New(t)
	fx := newIdentityDiscoveryFixture(t)

	page, err := fx.Store.ScanIdentityDiscoveryPageContext(t.Context(), fx.SourceID, 0, 100)
	require.NoError(t, err)

	assert.Equal(int64(3), page.Scanned)
	assert.Equal(fx.GmailSentID, page.NextAfterID)
	assert.ElementsMatch([]store.IdentityObservation{
		{MessageID: fx.SentID, Identifier: "Masked-Shop@Example.test", RecipientType: "from", HasSentFolder: true, ObservedAt: fx.When},
		{MessageID: fx.SentID, Identifier: "second-from@example.test", RecipientType: "from", HasSentFolder: true, ObservedAt: fx.When},
		{MessageID: fx.SentID, Identifier: "sent-to@example.test", RecipientType: "to", HasSentFolder: true, ObservedAt: fx.When},
		{MessageID: fx.SentID, Identifier: "sent-cc@example.test", RecipientType: "cc", HasSentFolder: true, ObservedAt: fx.When},
		{MessageID: fx.SentID, Identifier: "sent-bcc@example.test", RecipientType: "bcc", HasSentFolder: true, ObservedAt: fx.When},
		{MessageID: fx.InboundID, Identifier: "inbound-only@example.test", RecipientType: "to", ObservedAt: fx.When.Add(time.Hour)},
		{MessageID: fx.GmailSentID, Identifier: "gmail-sender@example.test", RecipientType: "from", IsFromMe: true, HasSentLabel: true, ObservedAt: fx.When.Add(2 * time.Hour)},
	}, page.Observations)
}

func TestIdentityDiscoveryScanReadsOnlySourceNativeAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	senderID := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	require.NoError(
		f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"),
		"confirm identity before ingestion",
	)
	messageID, err := f.Store.UpsertMessage(&store.Message{
		ConversationID:  f.ConvID,
		SourceID:        f.Source.ID,
		SourceMessageID: "identity-derived-message",
		MessageType:     "email",
		SenderID:        sql.NullInt64{Int64: senderID, Valid: true},
		SizeEstimate:    100,
	})
	require.NoError(err, "persist identity-derived message")
	require.NoError(
		f.Store.ReplaceMessageRecipients(messageID, "from", []int64{senderID}, []string{"Owner"}),
		"add From participant",
	)

	var isFromMe, sourceIsFromMe, identityIsFromMe bool
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT is_from_me, source_is_from_me, identity_is_from_me
		FROM messages
		WHERE id = ?
	`), messageID).Scan(&isFromMe, &sourceIsFromMe, &identityIsFromMe))
	require.True(isFromMe, "fixture must carry effective attribution")
	require.False(sourceIsFromMe, "fixture must not carry provider-native attribution")
	require.True(identityIsFromMe, "fixture attribution must be identity-derived")

	page, err := f.Store.ScanIdentityDiscoveryPageContext(t.Context(), f.Source.ID, 0, 100)
	require.NoError(err, "scan identity discovery page")
	require.NotEmpty(page.Observations)
	for _, observation := range page.Observations {
		assert.False(observation.IsFromMe,
			"page scan must not treat identity-derived attribution as evidence")
	}

	batch, err := f.Store.ScanIdentityObservationsForSourceMessageIDsContext(
		t.Context(), f.Source.ID, []string{"identity-derived-message"})
	require.NoError(err, "scan identity observations for source message IDs")
	require.NotEmpty(batch)
	for _, observation := range batch {
		assert.False(observation.IsFromMe,
			"batch scan must not treat identity-derived attribution as evidence")
	}
}

func TestIdentityDiscoveryScanReportsEnvelopeAddressAfterParticipantMerge(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	aliceID := f.EnsureParticipant("alice@example.test", "Alice", "example.test")
	bobID := f.EnsureParticipant("bob@example.test", "Bob", "example.test")

	messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			ConversationID:  f.ConvID,
			SourceID:        f.Source.ID,
			SourceMessageID: "native-sent-from-alice",
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC), Valid: true},
			SenderID:        sql.NullInt64{Int64: aliceID, Valid: true},
			IsFromMe:        true,
			SizeEstimate:    100,
		},
		Recipients: []store.RecipientSet{{
			Type:           "from",
			ParticipantIDs: []int64{aliceID},
			DisplayNames:   []string{"Alice"},
			EmailAddresses: []string{"alice@example.test"},
		}},
	})
	require.NoError(err, "persist source-native sent message")

	require.NoError(f.Store.MergeParticipants(aliceID, bobID), "merge alice into bob")

	page, err := f.Store.ScanIdentityDiscoveryPageContext(t.Context(), f.Source.ID, 0, 100)
	require.NoError(err, "scan identity discovery page")
	require.Len(page.Observations, 1)
	observation := page.Observations[0]
	assert.Equal(messageID, observation.MessageID)
	assert.Equal("from", observation.RecipientType)
	assert.True(observation.IsFromMe, "provider-native attribution must survive the merge")
	assert.Equal("alice@example.test", observation.Identifier,
		"discovery must report the envelope address, not the merge survivor's primary email")

	batch, err := f.Store.ScanIdentityObservationsForSourceMessageIDsContext(
		t.Context(), f.Source.ID, []string{"native-sent-from-alice"})
	require.NoError(err, "scan identity observations for source message IDs")
	require.Len(batch, 1)
	assert.Equal("alice@example.test", batch[0].Identifier,
		"the ingestion-batch scan must report the envelope address after a merge")
}

// TestParticipantMergeKeepsDistinctEnvelopeAliasRows pins the merge collision
// rule to the full unique key: an absorbed participant's recipient row whose
// envelope address differs from every surviving row's must survive the
// repoint, while a row that matches a surviving snapshot (case-insensitively)
// is still deduplicated. Without this, merging an alias participant into the
// primary one destroyed the alias's envelope evidence, and identity discovery
// could no longer observe the alias at all.
func TestParticipantMergeKeepsDistinctEnvelopeAliasRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	aliasID := f.EnsureParticipant("alias@example.test", "Alias", "example.test")
	primaryID := f.EnsureParticipant("primary@example.test", "Primary", "example.test")

	persist := func(sourceMessageID string, emails ...string) int64 {
		t.Helper()
		participantIDs := []int64{aliasID, primaryID}
		id, err := f.Store.PersistMessage(&store.MessagePersistData{
			Message: &store.Message{
				ConversationID:  f.ConvID,
				SourceID:        f.Source.ID,
				SourceMessageID: sourceMessageID,
				MessageType:     "email",
				SentAt:          sql.NullTime{Time: time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC), Valid: true},
				SizeEstimate:    100,
			},
			Recipients: []store.RecipientSet{{
				Type:           "to",
				ParticipantIDs: participantIDs,
				DisplayNames:   []string{"Alias", "Primary"},
				EmailAddresses: emails,
			}},
		})
		require.NoError(err, "persist %s", sourceMessageID)
		return id
	}

	// Both addresses appear in one To: header as distinct participants.
	distinctID := persist("distinct-envelopes", "alias@example.test", "primary@example.test")
	// Both participants snapshot the same address (case variant): the
	// absorbed row must be deduplicated by the merge, not duplicated.
	sharedID := persist("shared-envelope", "Shared@Example.test", "shared@example.test")

	require.NoError(f.Store.MergeParticipants(aliasID, primaryID), "merge alias into primary")

	readEnvelopes := func(messageID int64) []string {
		t.Helper()
		rows, err := f.Store.DB().Query(f.Store.Rebind(`
			SELECT participant_id, email_address FROM message_recipients
			WHERE message_id = ? AND recipient_type = 'to' ORDER BY id
		`), messageID)
		require.NoError(err, "read recipient rows")
		defer func() { _ = rows.Close() }()
		var emails []string
		for rows.Next() {
			var participantID int64
			var email string
			require.NoError(rows.Scan(&participantID, &email), "scan recipient row")
			assert.Equal(primaryID, participantID, "every surviving row must point at the merge survivor")
			emails = append(emails, email)
		}
		require.NoError(rows.Err(), "iterate recipient rows")
		return emails
	}

	assert.ElementsMatch(
		[]string{"alias@example.test", "primary@example.test"},
		readEnvelopes(distinctID),
		"distinct envelope snapshots must both survive the merge")
	assert.Len(readEnvelopes(sharedID), 1,
		"case-variant duplicates of one envelope address must still collapse to a single row")

	page, err := f.Store.ScanIdentityDiscoveryPageContext(t.Context(), f.Source.ID, 0, 100)
	require.NoError(err, "scan identity discovery page")
	observed := make([]string, 0, len(page.Observations))
	for _, observation := range page.Observations {
		if observation.MessageID == distinctID {
			observed = append(observed, observation.Identifier)
		}
	}
	assert.ElementsMatch([]string{"alias@example.test", "primary@example.test"}, observed,
		"discovery must still observe both envelope addresses after the merge")
}

// mergedAliasEnvelopeFixture persists a non-source-native sent message whose
// sender is an alias participant, then merges that participant into the
// primary one — after which the alias survives ONLY in the message's 'from'
// envelope snapshot. Confirming the alias afterwards must still repair the
// message's identity attribution.
func mergedAliasEnvelopeFixture(t *testing.T) (*storetest.Fixture, int64) {
	t.Helper()
	f := storetest.New(t)

	aliasID := f.EnsureParticipant("alias@example.test", "Alias", "example.test")
	primaryID := f.EnsureParticipant("primary@example.test", "Primary", "example.test")

	messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			ConversationID:  f.ConvID,
			SourceID:        f.Source.ID,
			SourceMessageID: "sent-from-merged-alias",
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC), Valid: true},
			SenderID:        sql.NullInt64{Int64: aliasID, Valid: true},
			SizeEstimate:    100,
		},
		Recipients: []store.RecipientSet{{
			Type:           "from",
			ParticipantIDs: []int64{aliasID},
			DisplayNames:   []string{"Alias"},
			EmailAddresses: []string{"alias@example.test"},
		}},
	})
	require.NoError(t, err, "persist message sent from alias")

	require.NoError(t, f.Store.MergeParticipants(aliasID, primaryID), "merge alias into primary")

	var isFromMe, identityIsFromMe bool
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT is_from_me, identity_is_from_me FROM messages WHERE id = ?
	`), messageID).Scan(&isFromMe, &identityIsFromMe), "read pre-confirmation attribution")
	require.False(t, isFromMe, "fixture message must start unattributed")
	require.False(t, identityIsFromMe, "fixture message must start without identity attribution")

	return f, messageID
}

func assertMessageIdentityAttributed(t *testing.T, f *storetest.Fixture, messageID int64) {
	t.Helper()
	var isFromMe, sourceIsFromMe, identityIsFromMe bool
	require.NoError(t, f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT is_from_me, source_is_from_me, identity_is_from_me FROM messages WHERE id = ?
	`), messageID).Scan(&isFromMe, &sourceIsFromMe, &identityIsFromMe), "read attribution")
	assert.True(t, identityIsFromMe,
		"confirming the alias must attribute the message through its envelope snapshot")
	assert.True(t, isFromMe, "effective attribution must follow the identity repair")
	assert.False(t, sourceIsFromMe, "source-native provenance must stay untouched")
}

func TestBatchConfirmationAttributesMergedAliasEnvelopeMessages(t *testing.T) {
	require := require.New(t)
	f, messageID := mergedAliasEnvelopeFixture(t)

	outcomes, err := f.Store.AddAccountIdentitiesBatchContext(
		t.Context(), f.Source.ID,
		[]store.IdentityConfirmation{{Identifier: "alias@example.test", Signals: []string{"sent-folder"}}},
	)
	require.NoError(err, "confirm merged alias in batch")
	require.Len(outcomes, 1)
	require.True(outcomes[0].Added, "batch confirmation must insert the alias identity")

	assertMessageIdentityAttributed(t, f, messageID)
}

func TestAddAccountIdentityAttributesMergedAliasEnvelopeMessages(t *testing.T) {
	require := require.New(t)
	f, messageID := mergedAliasEnvelopeFixture(t)

	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "alias@example.test", "manual"),
		"confirm merged alias")

	assertMessageIdentityAttributed(t, f, messageID)
}

// TestPersistMessageAttributesEnvelopeOnlyIdentity pins the persist-time
// ordering: the message upsert computes attribution from the sender
// participant before this transaction writes the 'from' envelope snapshot,
// so a confirmed identity represented only in that snapshot must still come
// out attributed once the recipient rows are final.
func TestPersistMessageAttributesEnvelopeOnlyIdentity(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	primaryID := f.EnsureParticipant("primary@example.test", "Primary", "example.test")
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "alias@example.test", "manual"),
		"confirm alias no participant carries")

	messageID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			ConversationID:  f.ConvID,
			SourceID:        f.Source.ID,
			SourceMessageID: "envelope-only-identity",
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: time.Date(2026, 8, 4, 9, 0, 0, 0, time.UTC), Valid: true},
			SenderID:        sql.NullInt64{Int64: primaryID, Valid: true},
			SizeEstimate:    100,
		},
		Recipients: []store.RecipientSet{{
			Type:           "from",
			ParticipantIDs: []int64{primaryID},
			DisplayNames:   []string{"Primary"},
			EmailAddresses: []string{"alias@example.test"},
		}},
	})
	require.NoError(err, "persist message with envelope-only identity")

	assertMessageIdentityAttributed(t, f, messageID)
}

// TestRepersistKeepsMergedAliasEnvelopeAttribution re-persists a repaired
// message the way a repair re-sync would — sender resolved to the merge
// survivor, the 'from' envelope still carrying the confirmed alias — and
// requires the repaired attribution to survive: the upsert's ON CONFLICT
// recomputation cannot see envelope rows and would otherwise clear it.
func TestRepersistKeepsMergedAliasEnvelopeAttribution(t *testing.T) {
	require := require.New(t)
	f, messageID := mergedAliasEnvelopeFixture(t)

	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "alias@example.test", "manual"),
		"confirm merged alias")
	assertMessageIdentityAttributed(t, f, messageID)

	primaryID := f.EnsureParticipant("primary@example.test", "Primary", "example.test")
	repersistedID, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			ConversationID:  f.ConvID,
			SourceID:        f.Source.ID,
			SourceMessageID: "sent-from-merged-alias",
			MessageType:     "email",
			SentAt:          sql.NullTime{Time: time.Date(2026, 8, 3, 9, 0, 0, 0, time.UTC), Valid: true},
			SenderID:        sql.NullInt64{Int64: primaryID, Valid: true},
			SizeEstimate:    100,
		},
		Recipients: []store.RecipientSet{{
			Type:           "from",
			ParticipantIDs: []int64{primaryID},
			DisplayNames:   []string{"Primary"},
			EmailAddresses: []string{"alias@example.test"},
		}},
	})
	require.NoError(err, "re-persist repaired message")
	require.Equal(messageID, repersistedID, "re-persist must land on the same message row")

	assertMessageIdentityAttributed(t, f, messageID)
}

func TestIdentityDiscoveryCountAndPageUseDistinctMessageKeyset(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fx := newIdentityDiscoveryFixture(t)

	count, err := fx.Store.CountIdentityDiscoveryMessagesContext(t.Context(), fx.SourceID)
	require.NoError(err)
	assert.Equal(int64(3), count, "count must not multiply messages by recipients or labels")

	first, err := fx.Store.ScanIdentityDiscoveryPageContext(t.Context(), fx.SourceID, 0, 2)
	require.NoError(err)
	assert.Equal(int64(2), first.Scanned)
	assert.Equal(fx.InboundID, first.NextAfterID)
	assert.NotEmpty(first.Observations)
	for _, observation := range first.Observations {
		assert.LessOrEqual(observation.MessageID, fx.InboundID)
	}

	second, err := fx.Store.ScanIdentityDiscoveryPageContext(t.Context(), fx.SourceID, first.NextAfterID, 2)
	require.NoError(err)
	assert.Equal(int64(1), second.Scanned)
	assert.Equal(fx.GmailSentID, second.NextAfterID)
	require.Len(second.Observations, 1)
	assert.Equal(fx.GmailSentID, second.Observations[0].MessageID)
}

func TestIdentityDiscoverySourceMessageIDsAreSourceScopedAndBounded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	fx := newIdentityDiscoveryFixture(t)
	other, err := fx.Store.GetOrCreateSource("imap", "other@example.test")
	require.NoError(err)
	otherConv, err := fx.Store.EnsureConversation(other.ID, "other-thread", "Other")
	require.NoError(err)
	otherMessageID, err := fx.Store.UpsertMessage(&store.Message{
		ConversationID: otherConv, SourceID: other.ID, SourceMessageID: "sent-folder-message",
		MessageType: "email", SizeEstimate: 100,
	})
	require.NoError(err)
	otherParticipant, err := fx.Store.EnsureParticipant("other-source@example.test", "Other", "example.test")
	require.NoError(err)
	require.NoError(
		fx.Store.ReplaceMessageRecipients(otherMessageID, "from", []int64{otherParticipant}, []string{"Other"}))
	imapUpperSentLabel, err := fx.Store.EnsureLabel(other.ID, "SENT", "Uppercase mailbox", "system")
	require.NoError(err)
	require.NoError(fx.Store.ReplaceMessageLabels(otherMessageID, []int64{imapUpperSentLabel}))

	sourceMessageIDs := make([]string, 0, 503)
	for i := range 501 {
		sourceMessageIDs = append(sourceMessageIDs, fmt.Sprintf("missing-%03d", i))
	}
	sourceMessageIDs = append(sourceMessageIDs, "sent-folder-message", "inbound-message")

	observations, err := fx.Store.ScanIdentityObservationsForSourceMessageIDsContext(
		t.Context(), fx.SourceID, sourceMessageIDs)
	require.NoError(err)
	assert.NotEmpty(observations)
	for _, observation := range observations {
		assert.Contains([]int64{fx.SentID, fx.InboundID}, observation.MessageID)
		assert.NotEqual("other-source@example.test", observation.Identifier)
	}

	otherObservations, err := fx.Store.ScanIdentityObservationsForSourceMessageIDsContext(
		t.Context(), other.ID, []string{"sent-folder-message"})
	require.NoError(err)
	require.Len(otherObservations, 1)
	assert.False(otherObservations[0].HasSentLabel,
		"an IMAP mailbox ID named SENT is not canonical Gmail sent evidence")
}

func TestIdentityDiscoveryDoesNotReadContentTables(t *testing.T) {
	fx := newIdentityDiscoveryFixture(t)
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true, MaxStmtChars: 10_000})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })

	var logBuffer bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	_, err := fx.Store.ScanIdentityDiscoveryPageContext(context.Background(), fx.SourceID, 0, 100)
	require.NoError(t, err)

	statements := strings.ToLower(logBuffer.String())
	assert.Contains(t, statements, "message_recipients", "trace must observe the production metadata query")
	for _, forbidden := range []string{"message_bodies", "message_raw", "attachments", "snippet"} {
		assert.NotContains(t, statements, forbidden)
	}
}

func TestAccountIdentityBatchDeterministicallyMergesCaseAndSignals(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	beforeIdentityRevision, err := f.Store.IdentityRevision()
	require.NoError(err)
	beforeAccountRevision, err := f.Store.AccountIdentityRevision()
	require.NoError(err)

	outcomes, err := f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, []store.IdentityConfirmation{
		{Identifier: "Zulu@example.test", Signals: []string{"sent-label"}},
		{Identifier: "Alias@Example.test", Signals: []string{"sent-folder", "is_from_me"}},
		{Identifier: "alias@example.test", Signals: []string{"sent-label", "is_from_me"}},
		{Identifier: "zulu@example.test", Signals: []string{"is_from_me"}},
	})
	require.NoError(err)
	assert.Equal([]store.IdentityConfirmationOutcome{
		{Identifier: "Alias@Example.test", Added: true, Signals: []string{"is_from_me", "sent-folder", "sent-label"}},
		{Identifier: "Zulu@example.test", Added: true, Signals: []string{"is_from_me", "sent-label"}},
	}, outcomes)

	identities, err := f.Store.ListAccountIdentities(f.Source.ID)
	require.NoError(err)
	require.Len(identities, 2)
	assert.Equal("Alias@Example.test", identities[0].Address, "first spelling must win case-folded batching")
	assert.Equal("is_from_me,sent-folder,sent-label", identities[0].SourceSignal)
	assert.Equal("Zulu@example.test", identities[1].Address)
	assert.Equal("is_from_me,sent-label", identities[1].SourceSignal)

	afterIdentityRevision, err := f.Store.IdentityRevision()
	require.NoError(err)
	afterAccountRevision, err := f.Store.AccountIdentityRevision()
	require.NoError(err)
	assert.Equal(beforeIdentityRevision+1, afterIdentityRevision, "one chunk with two inserts bumps once")
	assert.Equal(beforeAccountRevision+1, afterAccountRevision, "one chunk with two inserts bumps once")
}

func TestAccountIdentityBatchRefreshesExistingMessageAttribution(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	senderID := f.EnsureParticipant("masked-shop@example.test", "Masked Shop", "example.test")
	message := f.NewMessage().WithSourceMessageID("sent-before-alias-confirmation").Build()
	message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(err, "persist message before alias confirmation")

	before, err := f.Store.GetMessageIsFromMe(messageID)
	require.NoError(err, "read initial attribution")
	assert.False(before)

	_, err = f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, []store.IdentityConfirmation{{
		Identifier: "MASKED-SHOP@EXAMPLE.TEST",
		Signals:    []string{"sent-folder"},
	}})
	require.NoError(err, "confirm alias in batch")

	var isFromMe, sourceIsFromMe, identityIsFromMe bool
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT is_from_me, source_is_from_me, identity_is_from_me
		FROM messages
		WHERE id = ?
	`), messageID).Scan(&isFromMe, &sourceIsFromMe, &identityIsFromMe))
	assert.True(isFromMe, "batch confirmation must repair the existing message")
	assert.False(sourceIsFromMe, "message did not carry source-native attribution")
	assert.True(identityIsFromMe, "batch confirmation must retain identity provenance")
}

func TestAccountIdentityBatchBoundsLargeWritesAndKeepsRetryIdempotent(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	confirmations := make([]store.IdentityConfirmation, 501)
	for i := range confirmations {
		confirmations[i] = store.IdentityConfirmation{
			Identifier: fmt.Sprintf("masked-%03d@example.test", i),
			Signals:    []string{"sent-folder"},
		}
	}
	beforeIdentityRevision, err := f.Store.IdentityRevision()
	requirements.NoError(err)
	beforeAccountRevision, err := f.Store.AccountIdentityRevision()
	requirements.NoError(err)

	first, err := f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, confirmations)
	requirements.NoError(err)
	requirements.Len(first, 501)
	afterFirstIdentityRevision, err := f.Store.IdentityRevision()
	requirements.NoError(err)
	afterFirstAccountRevision, err := f.Store.AccountIdentityRevision()
	requirements.NoError(err)
	assertions.Equal(beforeIdentityRevision+2, afterFirstIdentityRevision,
		"501 inserts must commit as one 500-row chunk and one 1-row chunk")
	assertions.Equal(beforeAccountRevision+2, afterFirstAccountRevision,
		"each insert-bearing chunk must bump the account identity revision once")

	retry, err := f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, confirmations)
	requirements.NoError(err)
	requirements.Len(retry, 501)
	for _, outcome := range retry {
		assertions.False(outcome.Added, outcome.Identifier)
	}
	afterRetryIdentityRevision, err := f.Store.IdentityRevision()
	requirements.NoError(err)
	afterRetryAccountRevision, err := f.Store.AccountIdentityRevision()
	requirements.NoError(err)
	assertions.Equal(afterFirstIdentityRevision, afterRetryIdentityRevision,
		"an identical retry must not bump identity revision")
	assertions.Equal(afterFirstAccountRevision, afterRetryAccountRevision,
		"an identical retry must not bump account identity revision")
}

func TestAccountIdentityBatchPreservesExistingSpellingTimestampAndRevisions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "First@Example.test", "manual"))
	beforeRows, err := f.Store.ListAccountIdentities(f.Source.ID)
	require.NoError(err)
	require.Len(beforeRows, 1)
	beforeIdentityRevision, err := f.Store.IdentityRevision()
	require.NoError(err)
	beforeAccountRevision, err := f.Store.AccountIdentityRevision()
	require.NoError(err)

	outcomes, err := f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, []store.IdentityConfirmation{
		{Identifier: "first@example.test", Signals: []string{"sent-folder", "manual"}},
		{Identifier: "FIRST@example.test", Signals: []string{"is_from_me"}},
	})
	require.NoError(err)
	assert.Equal([]store.IdentityConfirmationOutcome{{
		Identifier: "first@example.test", Added: false, Signals: []string{"is_from_me", "manual", "sent-folder"},
	}}, outcomes)

	afterRows, err := f.Store.ListAccountIdentities(f.Source.ID)
	require.NoError(err)
	require.Len(afterRows, 1)
	assert.Equal("First@Example.test", afterRows[0].Address)
	assert.Equal("is_from_me,manual,sent-folder", afterRows[0].SourceSignal)
	assert.True(afterRows[0].ConfirmedAt.Equal(beforeRows[0].ConfirmedAt), "confirmed_at must stay at first confirmation")
	afterIdentityRevision, err := f.Store.IdentityRevision()
	require.NoError(err)
	afterAccountRevision, err := f.Store.AccountIdentityRevision()
	require.NoError(err)
	assert.Equal(beforeIdentityRevision, afterIdentityRevision, "signal-only merge must not bump")
	assert.Equal(beforeAccountRevision, afterAccountRevision, "signal-only merge must not bump")
}

func TestAccountIdentityBatchRejectsInvalidSignalBeforeWriting(t *testing.T) {
	f := storetest.New(t)

	_, err := f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, []store.IdentityConfirmation{
		{Identifier: "safe@example.test", Signals: []string{"sent-folder"}},
		{Identifier: "invalid@example.test", Signals: []string{"sent,label"}},
	})
	require.ErrorContains(t, err, "comma")

	identities, listErr := f.Store.ListAccountIdentities(f.Source.ID)
	require.NoError(t, listErr)
	assert.Empty(t, identities, "validation must finish before committing any chunk")
}

func TestAccountIdentityBatchRefreshesOnlyAffectedSenders(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	aliceID := f.EnsureParticipant("alice@example.test", "Alice", "example.test")
	bobID := f.EnsureParticipant("bob@example.test", "Bob", "example.test")

	aliceMessage := f.NewMessage().WithSourceMessageID("alice-message").Build()
	aliceMessage.SenderID = sql.NullInt64{Int64: aliceID, Valid: true}
	aliceMessageID, err := f.Store.UpsertMessage(aliceMessage)
	require.NoError(err, "persist alice message")

	bobMessage := f.NewMessage().WithSourceMessageID("bob-message").Build()
	bobMessage.SenderID = sql.NullInt64{Int64: bobID, Valid: true}
	bobMessageID, err := f.Store.UpsertMessage(bobMessage)
	require.NoError(err, "persist bob message")

	// Confirm Bob first so his message starts out correctly attributed.
	_, err = f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, []store.IdentityConfirmation{
		{Identifier: "bob@example.test", Signals: []string{"sent-folder"}},
	})
	require.NoError(err, "confirm bob")

	bobIsFromMe, err := f.Store.GetMessageIsFromMe(bobMessageID)
	require.NoError(err)
	require.True(bobIsFromMe, "bob's own confirmation must repair his message first")

	// Remove Bob's identity row directly, leaving his message row untouched
	// (stale). A full-source refresh triggered by Alice's confirmation below
	// would recompute Bob's attribution from scratch, find no matching
	// identity, and flip his message back to false. A refresh scoped to
	// Alice's own participant must never visit Bob's row.
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`DELETE FROM account_identities WHERE source_id = ? AND address = ?`),
		f.Source.ID, "bob@example.test")
	require.NoError(err, "simulate a stale bob identity")

	_, err = f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, []store.IdentityConfirmation{
		{Identifier: "alice@example.test", Signals: []string{"sent-folder"}},
	})
	require.NoError(err, "confirm alice")

	aliceIsFromMe, err := f.Store.GetMessageIsFromMe(aliceMessageID)
	require.NoError(err)
	assert.True(aliceIsFromMe, "alice's message must be repaired by her own confirmation")

	bobIsFromMeAfter, err := f.Store.GetMessageIsFromMe(bobMessageID)
	require.NoError(err)
	assert.True(bobIsFromMeAfter,
		"a refresh scoped to alice's chunk must not touch bob's message, even though "+
			"a full-source refresh would have flipped it back to false")
}

func TestAccountIdentityBatchWithUnseenAliasSkipsRefresh(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)

	// A message exists but was never observed under the alias being confirmed.
	knownSenderID := f.EnsureParticipant("known@example.test", "Known", "example.test")
	message := f.NewMessage().WithSourceMessageID("preexisting-message").Build()
	message.SenderID = sql.NullInt64{Int64: knownSenderID, Valid: true}
	messageID, err := f.Store.UpsertMessage(message)
	require.NoError(err, "persist message from an unrelated sender")

	// Confirm the known sender first so his message starts out attributed,
	// then remove his identity row directly, leaving his message row stale
	// (true). Confirming the unseen alias below must succeed but skip the
	// refresh entirely: if it triggered ANY broader refresh — even one
	// correctly scoped but accidentally reaching this unrelated sender —
	// recomputing from the now-empty identity set would flip his message
	// back to false. A false-before/false-after version of this test would
	// pass even under a full-source refresh and would not prove anything.
	_, err = f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, []store.IdentityConfirmation{
		{Identifier: "known@example.test", Signals: []string{"sent-folder"}},
	})
	require.NoError(err, "confirm known sender")
	before, err := f.Store.GetMessageIsFromMe(messageID)
	require.NoError(err)
	require.True(before, "known sender's own confirmation must repair his message first")

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`DELETE FROM account_identities WHERE source_id = ? AND address = ?`),
		f.Source.ID, "known@example.test")
	require.NoError(err, "simulate a stale known-sender identity")

	outcomes, err := f.Store.AddAccountIdentitiesBatchContext(t.Context(), f.Source.ID, []store.IdentityConfirmation{
		{Identifier: "never-seen@example.test", Signals: []string{"manual"}},
	})
	require.NoError(err, "confirming an identity with no matching participant must succeed")
	require.Len(outcomes, 1)
	assert.True(outcomes[0].Added)

	after, err := f.Store.GetMessageIsFromMe(messageID)
	require.NoError(err)
	assert.True(after,
		"no participant matches the confirmed alias, so the refresh must be skipped entirely — "+
			"even a full-source refresh would have flipped the known sender's stale message back to false")
}
