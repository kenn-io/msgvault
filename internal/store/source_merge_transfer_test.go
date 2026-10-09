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
	"go.kenn.io/msgvault/internal/testutil"
)

func mergeFixtureMessage(t *testing.T, st *store.Store, sourceID int64, chat, providerID, body string, minute int) (int64, int64) {
	t.Helper()
	require := require.New(t)
	conversation, err := st.EnsureConversation(sourceID, "!"+chat+":example.test", "Example chat")
	require.NoError(err)
	sender, err := st.EnsureParticipant("owner@example.test", "Example Owner", "example.test")
	require.NoError(err)
	message, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: conversation,
		SourceMessageID: providerID, MessageType: "beeper", SenderID: sql.NullInt64{Int64: sender, Valid: true},
		SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, minute, 0, 0, time.UTC), Valid: true}})
	require.NoError(err)
	require.NoError(st.UpsertMessageBody(message, sql.NullString{String: body, Valid: true}, sql.NullString{}))
	require.NoError(st.UpsertMessageRaw(message, []byte("synthetic raw "+body)))
	return message, conversation
}

func emailMergeFixtureMessage(
	t *testing.T,
	st *store.Store,
	sourceID int64,
	providerID string,
	recipients []string,
	includeEnvelope bool,
) int64 {
	t.Helper()
	sender, err := st.EnsureParticipant("sender@example.test", "Example Sender", "example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversationWithType(sourceID, "thread-example", "email_thread", "Example subject")
	require.NoError(t, err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conversation, SourceMessageID: providerID,
		MessageType: "email", SenderID: sql.NullInt64{Int64: sender, Valid: true},
		SentAt:  sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
		Subject: sql.NullString{String: "Example subject", Valid: true},
	})
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageBody(messageID,
		sql.NullString{String: "same body", Valid: true}, sql.NullString{}))
	for _, address := range recipients {
		participant, err := st.EnsureParticipant(address, "Example Recipient", "example.test")
		require.NoError(t, err)
		envelope := address
		if !includeEnvelope {
			envelope = ""
		}
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_recipients(
			message_id, participant_id, recipient_type, email_address
		) VALUES (?, ?, 'to', ?)`), messageID, participant, envelope)
		require.NoError(t, err)
	}
	return messageID
}

func TestMergeSourcesEmailFingerprintRequiresMatchingCompleteRecipients(t *testing.T) {
	tests := []struct {
		name            string
		destinationTo   []string
		incomingTo      []string
		includeEnvelope bool
		wantDuplicates  int64
	}{
		{name: "different recipients", destinationTo: []string{"alice@example.test"}, incomingTo: []string{"bob@example.test"}, includeEnvelope: true},
		{name: "missing recipient evidence", includeEnvelope: true},
		{name: "same complete recipients", destinationTo: []string{"alice@example.test"}, incomingTo: []string{"alice@example.test"}, includeEnvelope: true, wantDuplicates: 1},
		{name: "missing immutable envelope", destinationTo: []string{"alice@example.test"}, incomingTo: []string{"alice@example.test"}, includeEnvelope: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("gmail", "history@example.test")
			require.NoError(err)
			into, err := st.GetOrCreateSource("gmail", "live@example.test")
			require.NoError(err)
			emailMergeFixtureMessage(t, st, into.ID, "live-message", test.destinationTo, test.includeEnvelope)
			incoming := emailMergeFixtureMessage(t, st, from.ID, "history-message", test.incomingTo, test.includeEnvelope)

			result, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
				FromSourceID: from.ID, IntoSourceID: into.ID,
			})
			require.NoError(err)
			assert.Equal(test.wantDuplicates, result.DuplicatesHidden)
			var hidden bool
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT deleted_at IS NOT NULL FROM messages WHERE id = ?`), incoming).Scan(&hidden))
			assert.Equal(test.wantDuplicates == 1, hidden)
		})
	}
}

func TestMergeSourcesRFC822MatchKeepsHistoryVisibleWhenSurvivorLacksContent(t *testing.T) {
	tests := []struct {
		name                  string
		survivorMissingBody   bool
		historyHasMIMERawData bool
	}{
		{name: "history body text absent from survivor", survivorMissingBody: true},
		{name: "history MIME raw data absent from survivor", historyHasMIMERawData: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("gmail", "history@example.test")
			require.NoError(err)
			into, err := st.GetOrCreateSource("gmail", "live@example.test")
			require.NoError(err)
			recipients := []string{"recipient@example.test"}
			survivor := emailMergeFixtureMessage(t, st, into.ID, "live-message", recipients, true)
			history := emailMergeFixtureMessage(t, st, from.ID, "history-message", recipients, true)
			for _, messageID := range []int64{survivor, history} {
				_, err := st.DB().Exec(st.Rebind(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), "<complete-match@example.test>", messageID)
				require.NoError(err)
			}

			if test.survivorMissingBody {
				_, err := st.DB().Exec(st.Rebind(`DELETE FROM message_bodies WHERE message_id = ?`), survivor)
				require.NoError(err)
			}
			if test.historyHasMIMERawData {
				require.NoError(st.UpsertMessageRaw(history, []byte("From: sender@example.test\r\nSubject: Historical copy\r\n\r\nbody")))
			}

			result, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
				FromSourceID: from.ID,
				IntoSourceID: into.ID,
			})
			require.NoError(err)
			assert.Zero(result.DuplicatesHidden)
			var historyVisible bool
			require.NoError(st.DB().QueryRow(st.Rebind(
				`SELECT deleted_at IS NULL FROM messages WHERE id = ?`), history).Scan(&historyVisible))
			assert.True(historyVisible, "message content absent from the survivor must remain visible")
		})
	}
}

func TestMergeSourcesArchiveOnlyIMAPMessagesAreNotProviderTargets(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("imap", "history@example.test")
	require.NoError(err)
	into, err := st.GetOrCreateSource("imap", "live@example.test")
	require.NoError(err)
	conversation, err := st.EnsureConversationWithType(from.ID, "INBOX|1", "email_thread", "Example subject")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: from.ID, ConversationID: conversation, SourceMessageID: "INBOX|1",
		MessageType: "email", RFC822MessageID: sql.NullString{String: "<history@example.test>", Valid: true},
	})
	require.NoError(err)

	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: from.ID, IntoSourceID: into.ID,
	})
	require.NoError(err)
	providerMatch, err := st.GetMessageIDByRFC822ID(into.ID, "<history@example.test>")
	require.NoError(err)
	assert.Zero(providerMatch, "namespaced history must not satisfy a provider RFC822 lookup")

	changed, err := st.ReconcileSourceMessageSnapshot(t.Context(), into.ID, map[string]struct{}{})
	require.NoError(err)
	assert.Zero(changed, "a complete empty provider snapshot must leave archive-only history untouched")
	var deletedFromSource sql.NullTime
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT deleted_from_source_at FROM messages WHERE id = ?`), messageID).Scan(&deletedFromSource))
	assert.False(deletedFromSource.Valid)
}

func TestMessageIDsWithLabelContextExcludesArchiveOnlyMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("msmail", "history@example.test")
	require.NoError(err)
	into, err := st.GetOrCreateSource("msmail", "live@example.test")
	require.NoError(err)
	rfc822MessageID := "<shared@example.test>"
	recipients := []string{"recipient@example.test"}
	liveID := emailMergeFixtureMessage(t, st, into.ID, "provider-live", recipients, true)
	historyID := emailMergeFixtureMessage(t, st, from.ID, "provider-history", recipients, true)
	for _, id := range []int64{liveID, historyID} {
		_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), rfc822MessageID, id)
		require.NoError(err)
	}

	sourceLabelID, err := st.EnsureLabel(from.ID, "Inbox", "Inbox", "system")
	require.NoError(err)
	require.NoError(st.ReplaceMessageLabels(historyID, []int64{sourceLabelID}))
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: from.ID,
		IntoSourceID: into.ID,
	})
	require.NoError(err)

	labelID, err := st.EnsureLabel(into.ID, "Inbox", "Inbox", "system")
	require.NoError(err)
	ids, err := st.MessageIDsWithLabelContext(t.Context(), into.ID, labelID)
	require.NoError(err)
	assert.Equal(map[string]int64{"provider-live": liveID}, ids)
}

func TestRecomputeConversationStatsUsesOnlyLiveMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "stats@example.test")
	require.NoError(err)
	live, conversation := mergeFixtureMessage(t, st, source.ID, "stats-room", "live", "live body", 1)
	hidden, _ := mergeFixtureMessage(t, st, source.ID, "stats-room", "hidden", "hidden body", 2)
	removed, _ := mergeFixtureMessage(t, st, source.ID, "stats-room", "removed", "removed body", 3)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET snippet = 'live preview' WHERE id = ?`), live)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at = ? WHERE id = ?`), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), hidden)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at = ? WHERE id = ?`), time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), removed)
	require.NoError(err)

	require.NoError(st.RecomputeConversationStatsForConversationContext(t.Context(), conversation))
	var messageCount int64
	var lastMessageAt sql.NullTime
	var preview sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_count, last_message_at, last_message_preview
		FROM conversations WHERE id = ?`), conversation).Scan(&messageCount, &lastMessageAt, &preview))
	assert.Equal(int64(1), messageCount)
	assert.True(lastMessageAt.Valid)
	assert.Equal(time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC), lastMessageAt.Time.UTC())
	assert.Equal("live preview", preview.String)
}

func TestMergeSourcesTransfersAndPreservesEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	ctx := context.Background()
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	survivor, destChat := mergeFixtureMessage(t, st, into.ID, "shared", "200", "overlap", 1)
	duplicate, oldChat := mergeFixtureMessage(t, st, from.ID, "shared", "100", "overlap", 1)
	unique, uniqueChat := mergeFixtureMessage(t, st, from.ID, "disjoint", "200", "different", 2)
	repeated, _ := mergeFixtureMessage(t, st, from.ID, "shared", "101", "overlap", 3)
	require.NoError(st.AddAccountIdentity(from.ID, "owner@example.test", "manual"))
	sourceLabel, err := st.EnsureLabel(from.ID, "source-label", "Example label", "user")
	require.NoError(err)
	destLabel, err := st.EnsureLabel(into.ID, "dest-label", "Example label", "user")
	require.NoError(err)
	require.NoError(st.ReplaceMessageLabels(duplicate, []int64{sourceLabel}))
	var participant int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM participants WHERE email_address = 'owner@example.test'`).Scan(&participant))
	require.NoError(st.EnsureConversationParticipant(oldChat, participant, "member"))
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_recipients(message_id, participant_id, recipient_type, email_address) VALUES (?, ?, 'to', 'owner@example.test')`), duplicate, participant)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO reactions(message_id, participant_id, reaction_type, reaction_value) VALUES (?, ?, 'emoji', 'like')`), duplicate, participant)
	require.NoError(err)
	// A stored attachment is complete fingerprint evidence. Historical pending media is separate.
	for _, id := range []int64{survivor, duplicate} {
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO attachments(message_id, storage_path, content_hash, source_attachment_id, attachment_state) VALUES (?, 'ab/synthetic', ?, 'beeper:historical-media', 'stored')`), id, strings.Repeat("a", 64))
		require.NoError(err)
	}
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO attachments(message_id, storage_path, source_attachment_id, attachment_state) VALUES (?, '', 'beeper:pending-media', 'pending')`), unique)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET content_changed_at = ? WHERE id IN (?, ?, ?)`), time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), duplicate, unique, repeated)
	require.NoError(err)
	require.NoError(st.SetArchiveMarker(ctx, store.BeeperReanchorMarkerKey(into.ID), "synthetic anchors"))
	req := store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true}
	preview, err := st.MergeSourcesContext(ctx, req)
	require.NoError(err)
	assert.Equal(int64(3), preview.MessagesMoved)
	assert.Equal(int64(1), preview.DuplicatesHidden)
	assert.Zero(preview.AttachmentsCopied, "an existing legacy attachment hash needs no new copy")
	var sourceID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_id FROM messages WHERE id = ?`), unique).Scan(&sourceID))
	assert.Equal(from.ID, sourceID)
	req.DryRun = false
	result, err := st.MergeSourcesContext(ctx, req)
	require.NoError(err)
	preview.DryRun = false
	assert.Equal(preview, result)
	for _, id := range []int64{duplicate, unique, repeated} {
		var providerID string
		var watermark time.Time
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_id, source_message_id, content_changed_at FROM messages WHERE id = ?`), id).Scan(&sourceID, &providerID, &watermark))
		assert.Equal(into.ID, sourceID)
		assert.Contains(providerID, "msgvault-archive:")
		assert.Greater(watermark.Year(), 2020, "moving source ownership must reach the change feed")
		raw, err := st.GetMessageRaw(id)
		require.NoError(err)
		assert.Contains(string(raw), "synthetic raw")
	}
	var hidden bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_at IS NOT NULL FROM messages WHERE id = ?`), duplicate).Scan(&hidden))
	assert.True(hidden)
	var conversationID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id FROM messages WHERE id = ?`), unique).Scan(&conversationID))
	assert.Equal(uniqueChat, conversationID)
	for _, query := range []string{
		`SELECT COUNT(*) FROM message_labels WHERE message_id = ? AND label_id = ` + intString(destLabel),
		`SELECT COUNT(*) FROM message_recipients WHERE message_id = ?`,
		`SELECT COUNT(*) FROM reactions WHERE message_id = ?`,
		`SELECT COUNT(*) FROM attachments WHERE message_id = ?`,
	} {
		var count int
		require.NoError(st.DB().QueryRow(st.Rebind(query), survivor).Scan(&count))
		assert.Equal(1, count)
	}
	var owned bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_from_me FROM messages WHERE id = ?`), unique).Scan(&owned))
	assert.True(owned)
	var roster int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`), destChat).Scan(&roster))
	assert.Equal(1, roster)
	anchors, err := st.ListRecentMessagesForSource(into.ID, 20)
	require.NoError(err)
	require.Len(anchors, 1)
	assert.Equal("200", anchors[0].SourceMessageID)
	pending, err := st.ListBeeperPendingAttachmentMessages(into.ID)
	require.NoError(err)
	assert.Empty(pending, "historical media cannot be fetched from the destination provider")
	// New live IDs must never overwrite a moved row, including a disjoint chat.
	future, _ := mergeFixtureMessage(t, st, into.ID, "disjoint", "200", "future live", 4)
	assert.NotEqual(unique, future)
	_, marked, err := st.GetArchiveMarker(ctx, store.BeeperReanchorMarkerKey(into.ID))
	require.NoError(err)
	assert.True(marked)
	again, err := st.MergeSourcesContext(ctx, req)
	require.NoError(err)
	assert.True(again.AlreadyMerged)
	assert.Equal(result.MessagesMoved, again.MessagesMoved)
}

func TestMergeSourcesPreservesEquivalentIdentityTimestamp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("gmail", "history@example.test")
	require.NoError(err)
	into, err := st.GetOrCreateSource("gmail", "live@example.test")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(from.ID, "owner@example.test", "manual"))
	require.NoError(st.AddAccountIdentity(into.ID, "owner@example.test", "manual"))
	_, err = st.DB().Exec(st.Rebind(`UPDATE account_identities SET confirmed_at = ? WHERE source_id = ?`),
		"2026-01-01 00:00:00", from.ID)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE account_identities SET confirmed_at = ? WHERE source_id = ?`),
		"2026-01-01 00:00:00.000", into.ID)
	require.NoError(err)

	var before string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT CAST(confirmed_at AS TEXT) FROM account_identities WHERE source_id = ?`), into.ID).Scan(&before))
	if !st.IsPostgreSQL() {
		assert.Equal("2026-01-01 00:00:00.000", before)
	}

	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: from.ID, IntoSourceID: into.ID,
	})
	require.NoError(err)

	var after string
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT CAST(confirmed_at AS TEXT) FROM account_identities WHERE source_id = ?`), into.ID).Scan(&after))
	assert.Equal(before, after, "equal confirmation times should not rewrite SQLite's lexical timestamp format")
}

func intString(n int64) string { return strconv.FormatInt(n, 10) }

func TestMergeSourcesPreservesAmbiguousRepeatedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	mergeFixtureMessage(t, st, into.ID, "shared", "1", "repeated", 1)
	left, _ := mergeFixtureMessage(t, st, from.ID, "shared", "2", "repeated", 1)
	right, _ := mergeFixtureMessage(t, st, from.ID, "shared", "3", "repeated", 1)
	result, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)
	assert.Zero(result.DuplicatesHidden)
	for _, id := range []int64{left, right} {
		var live bool
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_at IS NULL FROM messages WHERE id = ?`), id).Scan(&live))
		assert.True(live, "identical content does not prove occurrence identity")
	}
}

func TestMergeSourcesNamespacesIMAPConversationIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("imap", "history@example.test")
	require.NoError(err)
	into, err := st.GetOrCreateSource("imap", "live@example.test")
	require.NoError(err)
	historical, err := st.EnsureConversation(from.ID, "INBOX|42", "Historical")
	require.NoError(err)
	current, err := st.EnsureConversation(into.ID, "INBOX|42", "Current")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: from.ID, ConversationID: historical, SourceMessageID: "INBOX|42", MessageType: "email"})
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)
	var conversationID int64
	var archivedID string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_id, c.source_conversation_id FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE m.source_id = ?`), into.ID).Scan(&conversationID, &archivedID))
	assert.Equal(historical, conversationID)
	assert.NotEqual(current, conversationID)
	assert.Contains(archivedID, "msgvault-archive:")
	future, err := st.EnsureConversation(into.ID, "INBOX|43", "Future")
	require.NoError(err)
	assert.NotEqual(historical, future)
}

func TestMergeSourcesPreservesDestinationAttachmentEligibility(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	var messages []int64
	for _, source := range []*store.Source{from, into} {
		message, _ := mergeFixtureMessage(
			t, st, source.ID, "shared", fmt.Sprintf("provider-message-%d", source.ID), "same body", 1)
		messages = append(messages, message)
		_, err := st.DB().Exec(st.Rebind(`INSERT INTO attachments(message_id, storage_path, content_hash, source_attachment_id, attachment_state) VALUES (?, '', ?, ?, 'pending')`), message, strings.Repeat("d", 64), fmt.Sprintf("beeper:receipt:%d", source.ID))
		require.NoError(err)
	}
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)
	var state string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT attachment_state FROM attachments WHERE message_id = ? AND source_attachment_id = ?`), messages[1], fmt.Sprintf("beeper:receipt:%d", into.ID)).Scan(&state))
	assert.Equal("pending", state, "original destination media remains fetchable")
	providerAttachments, err := st.MessageBeeperAttachments(messages[1])
	require.NoError(err)
	assert.Len(providerAttachments, 1)
	assert.Contains(providerAttachments, fmt.Sprintf("beeper:receipt:%d", into.ID))
	assert.NotContains(providerAttachments, fmt.Sprintf("beeper:receipt:%d", from.ID),
		"a preserved historical receipt cannot route through the destination account")
}

func mergedMIMEAttachmentFixture(
	t *testing.T,
) (*store.Store, int64, store.MessageIdentityGuard) {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("gmail", "mime-history@example.test")
	require.NoError(err)
	into, err := st.GetOrCreateSource("gmail", "mime-live@example.test")
	require.NoError(err)
	recipients := []string{"recipient@example.test"}
	survivorID := emailMergeFixtureMessage(t, st, into.ID, "live-message", recipients, true)
	historyID := emailMergeFixtureMessage(t, st, from.ID, "history-message", recipients, true)
	hash := strings.Repeat("b", 64)
	for messageID, partKey := range map[int64]string{
		survivorID: "mime:live", historyID: "mime:history",
	} {
		_, err = st.DB().Exec(st.Rebind(`
			INSERT INTO attachments(message_id, filename, mime_type, size, content_hash, storage_path, source_part_key)
			VALUES (?, 'historical.txt', 'text/plain', 10, ?, ?, ?)
		`), messageID, hash, hash[:2]+"/"+hash, partKey)
		require.NoError(err)
	}
	result, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: from.ID, IntoSourceID: into.ID,
	})
	require.NoError(err)
	require.Equal(int64(1), result.DuplicatesHidden)
	var copied int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM attachments a
		JOIN source_merge_attachments sa ON sa.attachment_id = a.id
		WHERE a.message_id = ?
	`), survivorID).Scan(&copied))
	require.Equal(1, copied)
	return st, survivorID, store.MessageIdentityGuard{
		ID: survivorID, SourceID: into.ID, SourceMessageID: "live-message",
	}
}

func TestInitSchemaContextBackfillsPortableSourceMergeMarkers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, _, _ := mergedMIMEAttachmentFixture(t)

	var archiveOnlyMessageID, preservedAttachmentID int64
	require.NoError(st.DB().QueryRow(`
		SELECT message_id FROM source_merge_messages
		WHERE archive_only = TRUE
		ORDER BY message_id LIMIT 1
	`).Scan(&archiveOnlyMessageID))
	require.NoError(st.DB().QueryRow(`
		SELECT attachment_id FROM source_merge_attachments
		ORDER BY attachment_id LIMIT 1
	`).Scan(&preservedAttachmentID))

	_, err := st.DB().Exec(`DROP TABLE source_merge_archive_only_messages`)
	require.NoError(err)
	_, err = st.DB().Exec(`DROP TABLE source_merge_preserved_attachments`)
	require.NoError(err)

	markerCount := func(table, column string, id int64) int {
		t.Helper()
		var count int
		query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = ?", table, column)
		require.NoError(st.DB().QueryRow(st.Rebind(query), id).Scan(&count))
		return count
	}
	require.NoError(st.InitSchemaContext(t.Context()))
	assert.Equal(1, markerCount("source_merge_archive_only_messages", "message_id", archiveOnlyMessageID))
	assert.Equal(1, markerCount("source_merge_preserved_attachments", "attachment_id", preservedAttachmentID))

	require.NoError(st.InitSchemaContext(t.Context()))
	assert.Equal(1, markerCount("source_merge_archive_only_messages", "message_id", archiveOnlyMessageID))
	assert.Equal(1, markerCount("source_merge_preserved_attachments", "attachment_id", preservedAttachmentID))
}

func TestSourceMergeHistoricalMIMEAttachmentsSurviveRefreshAndCleanup(t *testing.T) {
	tests := []struct {
		name    string
		refresh func(*testing.T, *store.Store, int64, store.MessageIdentityGuard)
	}{
		{
			name: "repair replaces survivor MIME",
			refresh: func(t *testing.T, st *store.Store, messageID int64, identity store.MessageIdentityGuard) {
				t.Helper()
				require := require.New(t)
				message, err := st.GetMessageContext(t.Context(), messageID)
				require.NoError(err)
				empty := []store.AttachmentWrite{}
				_, err = st.PersistRepairMessageWithParticipantsContext(
					t.Context(), identity, nil, func([]int64) *store.MessagePersistData {
						return &store.MessagePersistData{
							Message: &store.Message{
								SourceID: identity.SourceID, SourceMessageID: identity.SourceMessageID,
								ConversationID: message.ConversationID, MessageType: store.MessageTypeEmail,
								Subject: sql.NullString{String: message.Subject, Valid: message.Subject != ""},
							},
							BodyText:       sql.NullString{String: message.BodyText, Valid: true},
							PreserveLabels: true, MIMEAttachmentReplacement: &empty,
						}
					},
				)
				require.NoError(err)
			},
		},
		{
			name: "Microsoft refresh cleanup",
			refresh: func(t *testing.T, st *store.Store, messageID int64, _ store.MessageIdentityGuard) {
				t.Helper()
				require.NoError(t, st.DeleteMIMEAttachmentsExceptContext(t.Context(), messageID, nil))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, survivorID, identity := mergedMIMEAttachmentFixture(t)
			test.refresh(t, st, survivorID, identity)
			var filename string
			require.NoError(st.DB().QueryRow(st.Rebind(`
				SELECT a.filename FROM attachments a
				JOIN source_merge_attachments sa ON sa.attachment_id = a.id
				WHERE a.message_id = ?
			`), survivorID).Scan(&filename))
			assert.Equal("historical.txt", filename)
		})
	}
}

func TestSourceMergePreservedLegacyMIMEAttachmentSurvivesReplacement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("gmail", "legacy-history@example.test")
	require.NoError(err)
	into, err := st.GetOrCreateSource("gmail", "legacy-live@example.test")
	require.NoError(err)
	recipients := []string{"recipient@example.test"}
	survivorID := emailMergeFixtureMessage(t, st, into.ID, "live-message", recipients, true)
	historyID := emailMergeFixtureMessage(t, st, from.ID, "history-message", recipients, true)
	hash := strings.Repeat("c", 64)
	storagePath := hash[:2] + "/" + hash

	_, err = st.DB().Exec(st.Rebind(`
		INSERT INTO attachments(
			message_id, filename, mime_type, size, content_hash, storage_path,
			attachment_role, role_source, source_part_key
		) VALUES (?, 'current-old.txt', 'text/plain', 12, ?, ?, 'unknown', 'unknown', 'mime:current-old')
	`), survivorID, hash, storagePath)
	require.NoError(err)
	var historyAttachmentID int64
	err = st.DB().QueryRow(st.Rebind(`
		INSERT INTO attachments(
			message_id, filename, mime_type, size, content_hash, storage_path,
			attachment_role, role_source, source_part_key
		) VALUES (?, 'history-old.txt', 'text/plain', 12, ?, ?, 'unknown', 'unknown', NULL)
		RETURNING id
	`), historyID, hash, storagePath).Scan(&historyAttachmentID)
	require.NoError(err)

	result, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: from.ID, IntoSourceID: into.ID,
	})
	require.NoError(err)
	require.Equal(int64(1), result.DuplicatesHidden)

	var copiedAttachmentID int64
	var copiedFilename, copiedPartKey, copiedHash, copiedStoragePath string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT a.id, a.filename, COALESCE(a.source_part_key, ''), a.content_hash, a.storage_path
		FROM attachments a
		JOIN source_merge_attachments sa ON sa.attachment_id = a.id
		WHERE a.message_id = ? AND sa.original_attachment_id = ?
	`), survivorID, historyAttachmentID).Scan(
		&copiedAttachmentID, &copiedFilename, &copiedPartKey, &copiedHash, &copiedStoragePath,
	))
	assert.Equal("history-old.txt", copiedFilename)
	assert.Empty(copiedPartKey)
	assert.Equal(hash, copiedHash)
	assert.Equal(storagePath, copiedStoragePath)

	message, err := st.GetMessageContext(t.Context(), survivorID)
	require.NoError(err)
	replacement := []store.AttachmentWrite{{
		Filename: "replacement.txt", MIMEType: "text/plain", Size: 12,
		ContentHash: hash, StoragePath: storagePath,
		Role: store.AttachmentRoleUnknown, RoleSource: store.AttachmentRoleSourceUnknown,
		SourcePartKey: "mime:replacement",
	}}
	_, err = st.PersistRepairMessageWithParticipantsContext(
		t.Context(), store.MessageIdentityGuard{
			ID: survivorID, SourceID: into.ID, SourceMessageID: "live-message",
		}, nil, func([]int64) *store.MessagePersistData {
			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID: into.ID, SourceMessageID: "live-message",
					ConversationID: message.ConversationID, MessageType: store.MessageTypeEmail,
					Subject: sql.NullString{String: message.Subject, Valid: message.Subject != ""},
				},
				BodyText:       sql.NullString{String: message.BodyText, Valid: true},
				PreserveLabels: true, MIMEAttachmentReplacement: &replacement,
			}
		},
	)
	require.NoError(err)

	currentAttachments, err := st.MessageMIMEAttachmentsContext(t.Context(), survivorID)
	require.NoError(err)
	require.Len(currentAttachments, 1, "the replacement remains visible as current MIME")
	assert.Equal("replacement.txt", currentAttachments[0].Filename)
	assert.Equal("mime:replacement", currentAttachments[0].SourcePartKey)

	var finalFilename, finalPartKey, finalHash, finalStoragePath string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT filename, COALESCE(source_part_key, ''), content_hash, storage_path
		FROM attachments WHERE id = ?
	`), copiedAttachmentID).Scan(&finalFilename, &finalPartKey, &finalHash, &finalStoragePath))
	assert.Equal("history-old.txt", finalFilename)
	assert.Empty(finalPartKey)
	assert.Equal(hash, finalHash)
	assert.Equal(storagePath, finalStoragePath)
}

func TestMergeSourcesDoesNotHideLastLiveCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	old, _ := mergeFixtureMessage(t, st, into.ID, "shared", "1", "same", 1)
	live, _ := mergeFixtureMessage(t, st, from.ID, "shared", "2", "same", 1)
	deleted, _ := mergeFixtureMessage(t, st, from.ID, "shared", "3", "deleted", 2)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at = ?, delete_batch_id = 'original-batch' WHERE id IN (?, ?)`), time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), old, deleted)
	require.NoError(err)
	result, err := st.MergeSourcesContext(context.Background(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)
	assert.Zero(result.DuplicatesHidden)
	var visible bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_at IS NULL FROM messages WHERE id = ?`), live).Scan(&visible))
	assert.True(visible)
	for _, id := range []int64{old, deleted} {
		var batch string
		var stamp time.Time
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT delete_batch_id, deleted_at FROM messages WHERE id = ?`), id).Scan(&batch, &stamp))
		assert.Equal("original-batch", batch)
		assert.Equal(2021, stamp.Year())
	}
}

func TestMergeSourcesInvalidatesContextualEmbeddingsOnOwnershipChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	require.NoError(st.EnableEmbeddingChangeJournal(t.Context()))
	id, _ := mergeFixtureMessage(t, st, from.ID, "unique", "old", "body", 1)
	before, err := st.LatestEmbeddingChangeSequence(t.Context())
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)
	changes, err := st.ScanEmbeddingChanges(t.Context(), before, 100)
	require.NoError(err)
	found := false
	for _, change := range changes {
		if change.MessageID.Valid && change.MessageID.Int64 == id {
			found = true
		}
	}
	assert.True(found, "moving a conversation without changing its ID must invalidate embedding scope")
}

func TestMergeSourcesRejectsKnownDifferentBeeperNetworks(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	_, fromChat := mergeFixtureMessage(t, st, from.ID, "old-room", "old", "old body", 1)
	_, intoChat := mergeFixtureMessage(t, st, into.ID, "live-room", "new", "new body", 2)
	_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET metadata = ? WHERE id = ?`), `{"network":"signal"}`, fromChat)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET metadata = ? WHERE id = ?`), `{"network":"whatsapp"}`, intoChat)
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true})
	require.ErrorIs(err, store.ErrSourceMergeInvalid)
}

func TestMergeSourcesAllowsOverlappingBeeperNetworkFamilies(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	_, fromChat := mergeFixtureMessage(t, st, from.ID, "history-signal", "history-message", "history body", 1)
	_, intoSignalChat := mergeFixtureMessage(t, st, into.ID, "live-signal", "live-signal-message", "live signal body", 2)
	_, intoWhatsAppChat := mergeFixtureMessage(t, st, into.ID, "live-whatsapp", "live-whatsapp-message", "live WhatsApp body", 3)
	for _, item := range []struct {
		conversationID int64
		network        string
	}{
		{conversationID: fromChat, network: "signal"},
		{conversationID: intoSignalChat, network: "signal"},
		{conversationID: intoWhatsAppChat, network: "whatsapp"},
	} {
		_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET metadata = ? WHERE id = ?`),
			`{"network":"`+item.network+`"}`, item.conversationID)
		require.NoError(err)
	}

	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err, "a strict subset of the destination's recorded Beeper families is compatible")
	settings, err := st.GetSourceSettingsContext(t.Context(), from.ID)
	require.NoError(err)
	assert.Equal(into.ID, settings.MergedIntoSourceID)
	for _, conversationID := range []int64{fromChat, intoSignalChat, intoWhatsAppChat} {
		var sourceID int64
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_id FROM conversations WHERE id = ?`), conversationID).Scan(&sourceID))
		assert.Equal(into.ID, sourceID)
	}
}

func TestMergeSourcesRejectsKnownDifferentBeeperObservationServices(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-example")
	require.NoError(err)
	participant, err := st.EnsureParticipant("owner@example.test", "Example Owner", "example.test")
	require.NoError(err)
	for _, item := range []struct {
		sourceID int64
		service  string
	}{{from.ID, "signal"}, {into.ID, "whatsapp"}} {
		ref := fmt.Sprintf("beeper:example:%d", item.sourceID)
		_, err = st.RecordContactObservationContext(t.Context(), participant, store.ParticipantContactObservationInput{
			SourceID: &item.sourceID, AddressKind: store.ContactAddressPhone, ServiceSlug: &item.service, OriginalValue: "+12025550123",
			Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation, SourceRef: &ref},
		})
		require.NoError(err)
	}
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID, DryRun: true})
	require.ErrorIs(err, store.ErrSourceMergeInvalid)
}

func TestMergeSourcesCanConsolidatePreviouslyMergedHistory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource("beeper", "first-example")
	require.NoError(err)
	second, err := st.GetOrCreateSource("beeper", "second-example")
	require.NoError(err)
	third, err := st.GetOrCreateSource("beeper", "third-example")
	require.NoError(err)
	message, conversation := mergeFixtureMessage(t, st, first.ID, "historical-room", "provider-original", "body", 1)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: first.ID, IntoSourceID: second.ID})
	require.NoError(err)
	mergeFixtureMessage(t, st, third.ID, "historical-room", "live-provider", "body", 1)
	result, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: second.ID, IntoSourceID: third.ID})
	require.NoError(err)
	assert.Equal(int64(1), result.DuplicatesHidden, "stable original conversation identity survives earlier namespacing")
	var sourceID, original int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_id FROM messages WHERE id = ?`), message).Scan(&sourceID))
	assert.Equal(third.ID, sourceID)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT original_source_id FROM source_merge_conversations WHERE conversation_id = ?`), conversation).Scan(&original))
	assert.Equal(first.ID, original)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT original_source_id FROM source_merge_messages WHERE message_id = ?`), message).Scan(&original))
	assert.Equal(first.ID, original)
}

func TestMergeSourcesSequentialDedupIgnoresPreservedAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource("beeper", "first-example")
	require.NoError(err)
	second, err := st.GetOrCreateSource("beeper", "second-example")
	require.NoError(err)
	third, err := st.GetOrCreateSource("beeper", "third-example")
	require.NoError(err)

	hash := strings.Repeat("c", 64)
	var messageIDs []int64
	for _, source := range []*store.Source{first, second, third} {
		messageID, _ := mergeFixtureMessage(t, st, source.ID, "shared-room", "message-"+source.Identifier, "same body", 1)
		messageIDs = append(messageIDs, messageID)
		require.NoError(st.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
			Filename: "same.txt", MIMEType: "text/plain", StoragePath: hash[:2] + "/" + hash,
			ContentHash: hash, Size: 4, SourceAttachmentID: "beeper:attachment:" + source.Identifier,
			SourcePartKey: "attachment:" + source.Identifier,
		}))
	}
	require.Len(messageIDs, 3)

	firstMerge, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: first.ID, IntoSourceID: second.ID,
	})
	require.NoError(err)
	assert.Equal(int64(1), firstMerge.DuplicatesHidden)
	var attachmentCount, preservedAttachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`), messageIDs[1]).Scan(&attachmentCount))
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM source_merge_preserved_attachments spa
		JOIN attachments a ON a.id = spa.attachment_id WHERE a.message_id = ?
	`), messageIDs[1]).Scan(&preservedAttachmentCount))
	require.Equal(2, attachmentCount)
	require.Equal(1, preservedAttachmentCount)
	secondMerge, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: second.ID, IntoSourceID: third.ID,
	})
	require.NoError(err)
	assert.Equal(int64(1), secondMerge.DuplicatesHidden, "preserved attachment copies do not change the complete payload fingerprint")

	var hidden bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_at IS NOT NULL FROM messages WHERE id = ?`), messageIDs[1]).Scan(&hidden))
	assert.True(hidden, "the matched source copy is hidden when both fingerprints agree")
}

func TestSourceMergePortableMessageIdentityMigrationBackfillsFromAudit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newBeeperMediaFixture(t)
	destination, err := f.Store.GetOrCreateSource("beeper", "signal-main")
	require.NoError(err)
	audio := addBeeperAudio(t, f.Store, f.Source.ID, f.ConvID, "legacy-provider-message", strings.Repeat("a", 64))
	_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: f.Source.ID, IntoSourceID: destination.ID,
	})
	require.NoError(err)

	_, err = f.Store.DB().Exec(`ALTER TABLE source_merge_archive_only_messages DROP COLUMN original_source_message_id`)
	require.NoError(err)
	_, err = f.Store.DB().Exec(`DELETE FROM applied_migrations WHERE name = 'source_merge_archive_message_id_v1'`)
	require.NoError(err)
	require.NoError(f.Store.InitSchemaContext(t.Context()))

	candidate, err := f.Store.GetBeeperMediaCandidate(t.Context(), audio.attachmentID)
	require.NoError(err)
	assert.Equal("legacy-provider-message", candidate.OriginalSourceMessageID)
}

func TestMergeSourcesSupersedesDuplicateCurrentObservationsWithNullScopes(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("gmail", "history-observations@example.test")
	require.NoError(err)
	into, err := st.GetOrCreateSource("gmail", "live-observations@example.test")
	require.NoError(err)
	participant, err := st.EnsureParticipant("shared-observation@example.test", "Shared Example", "example.test")
	require.NoError(err)

	for _, source := range []*store.Source{from, into} {
		_, err = st.RecordContactObservationContext(t.Context(), participant, store.ParticipantContactObservationInput{
			SourceID: &source.ID, AddressKind: store.ContactAddressEmail,
			OriginalValue: "shared@example.test",
			Envelope:      store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
		})
		require.NoError(err)
	}

	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)

	current, err := st.ListParticipantObservationsContext(t.Context(), participant, true)
	require.NoError(err)
	assert.Len(current, 1, "matching source observations with NULL scope fields must collapse to one current row")
	all, err := st.ListParticipantObservationsContext(t.Context(), participant, false)
	require.NoError(err)
	assert.Len(all, 2, "the superseded observation must remain as history")
	var superseded int
	for _, observation := range all {
		if observation.Envelope.SupersededAt != nil {
			superseded++
		}
	}
	assert.Equal(1, superseded)
}

func TestMergeSourcesReconcilesObservationBackedIdentityMatches(t *testing.T) {
	for _, test := range []struct {
		name          string
		decidedBy     string
		wantCandidate bool
		wantLink      bool
	}{
		{name: "system decision is withdrawn"},
		{name: "user decision remains", decidedBy: "user", wantCandidate: true, wantLink: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("beeper", "history-match-"+strings.ReplaceAll(test.name, " ", "-"))
			require.NoError(err)
			into, err := st.GetOrCreateSource("beeper", "live-match-"+strings.ReplaceAll(test.name, " ", "-"))
			require.NoError(err)
			left, err := st.EnsureParticipantByIdentifier("beeper", "@history-match:example.test", "History Match")
			require.NoError(err)
			right, err := st.EnsureParticipantByIdentifier("beeper", "@live-match:example.test", "Live Match")
			require.NoError(err)

			providerID := "provider-user-shared"
			for _, participant := range []int64{left, right} {
				_, err = st.RecordContactObservationContext(t.Context(), participant, store.ParticipantContactObservationInput{
					SourceID: &from.ID, AddressKind: store.ContactAddressUsername,
					ServiceSlug: new("slack"), ScopeKind: new("workspace"), ScopeValue: new("T0EXAMPLE"),
					ProviderUserID: &providerID, OriginalValue: "shared-user",
					Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
				})
				require.NoError(err)
			}
			destinationProviderID := "different-provider-user"
			_, err = st.RecordContactObservationContext(t.Context(), left, store.ParticipantContactObservationInput{
				SourceID: &into.ID, AddressKind: store.ContactAddressUsername,
				ServiceSlug: new("slack"), ScopeKind: new("workspace"), ScopeValue: new("T0EXAMPLE"),
				ProviderUserID: &destinationProviderID, OriginalValue: "shared-user",
				Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceArchiveObservation},
			})
			require.NoError(err)

			candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: left,
				RightKind: store.IdentityMatchParticipant, RightID: right,
				Basis: store.IdentityMatchStableProviderID, NormalizedValue: &providerID,
				State:  store.IdentityMatchStateCandidate,
				Source: store.ProvenanceArchiveObservation, SourceID: &from.ID,
			})
			require.NoError(err)
			evidence, err := st.AddIdentityMatchEvidenceContext(t.Context(), candidate.ID, store.IdentityMatchEvidenceInput{
				EvidenceKind: "stable_provider_id", Detail: &providerID,
				Source: store.ProvenanceArchiveObservation, SourceID: &from.ID,
			})
			require.NoError(err)
			decidedBy := test.decidedBy
			if decidedBy == "" {
				decidedBy = "system"
			}
			_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, decidedBy, nil)
			require.NoError(err)
			require.True(linkedPair(t, st, left, right), "accepted candidate owns the participant link")

			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			require.NoError(err)

			remaining, err := st.GetIdentityMatchCandidateContext(t.Context(), candidate.ID)
			if test.wantCandidate {
				require.NoError(err)
				assert.Equal(store.IdentityMatchStateAccepted, remaining.State)
				assert.Equal(string(store.ProvenanceUser), *remaining.DecidedBy)
				assert.Empty(remaining.Evidence, "unsupported archive explanation must be removed")
			} else {
				require.ErrorIs(err, store.ErrIdentityMatchNotFound)
			}
			assert.Equal(test.wantLink, linkedPair(t, st, left, right))
			var evidenceCount int
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM identity_match_evidence WHERE id = ?`), evidence.ID).Scan(&evidenceCount))
			assert.Zero(evidenceCount)
		})
	}
}

func TestMergeSourcesRetainsEarlierDuplicateEvidence(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	first, err := st.GetOrCreateSource("beeper", "first-example")
	require.NoError(err)
	second, err := st.GetOrCreateSource("beeper", "second-example")
	require.NoError(err)
	third, err := st.GetOrCreateSource("beeper", "third-example")
	require.NoError(err)
	hidden, _ := mergeFixtureMessage(t, st, first.ID, "shared-room", "old-provider", "body", 1)
	survivor, _ := mergeFixtureMessage(t, st, second.ID, "shared-room", "live-provider", "body", 1)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: first.ID, IntoSourceID: second.ID})
	require.NoError(err)
	var basis string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT match_basis FROM source_merge_messages WHERE message_id = ?`), hidden).Scan(&basis))
	require.NotEmpty(basis)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: second.ID, IntoSourceID: third.ID})
	require.NoError(err)
	var keptBasis string
	var keptSurvivor int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT match_basis, survivor_message_id FROM source_merge_messages WHERE message_id = ?`), hidden).Scan(&keptBasis, &keptSurvivor))
	assert.Equal(basis, keptBasis)
	assert.Equal(survivor, keptSurvivor)
}

func TestRemoveSourcePreservesMergeHistory(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		for _, destination := range []bool{false, true} {
			t.Run(fmt.Sprintf("serialized=%t/destination=%t", serialized, destination), func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				st := testutil.NewTestStore(t)
				ctx := context.Background()
				from, err := st.GetOrCreateSource("beeper", "remove-history-example")
				require.NoError(err)
				into, err := st.GetOrCreateSource("beeper", "remove-live-example")
				require.NoError(err)
				_, err = st.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
				require.NoError(err)
				sourceID := from.ID
				if destination {
					sourceID = into.ID
				}
				if serialized {
					_, _, err = st.RemoveSourceSerialized(ctx, sourceID)
				} else {
					err = st.RemoveSource(sourceID)
				}
				require.ErrorIs(err, store.ErrSourceMergeParticipant)
				var count int
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&count))
				assert.Equal(2, count)
			})
		}
	}
}

func TestSlackMediaEnumerationExcludesMergedHistory(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("slack", "history-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("slack", "live-example")
	require.NoError(err)
	add := func(sourceID int64, channel, providerID string) int64 {
		t.Helper()
		conversation, err := st.EnsureConversation(sourceID, channel, "Example channel")
		require.NoError(err)
		message, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: conversation, SourceMessageID: providerID, MessageType: "slack"})
		require.NoError(err)
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO attachments(message_id, storage_path, source_attachment_id, attachment_state) VALUES (?, '', 'slack:file:EXAMPLE', 'pending')`), message)
		require.NoError(err)
		return message
	}
	historical := add(from.ID, "CEXAMPLEHISTORY", "1712345678.000001")
	live := add(into.ID, "CEXAMPLELIVE", "1712345678.000002")
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)
	pending, err := st.ListSlackPendingAttachmentMessages(into.ID)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(live, pending[0].MessageID)
	evidence, err := st.GetMessageSourceContext(t.Context(), historical)
	require.NoError(err)
	assert.Equal(into.ID, evidence.ID)
}
