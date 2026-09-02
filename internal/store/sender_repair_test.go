package store_test

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMissingMIMESenderRepair(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	malformedRaw := []byte("From: Fridgeco <noreply@fridgeco.example >\r\n" +
		"Subject: Repair me\r\n\r\nBody\r\n")
	repairable := f.NewMessage().
		WithSourceMessageID("missing-sender-repairable").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(repairable, malformedRaw),
		"UpsertMessageRaw repairable")

	headerless := f.NewMessage().
		WithSourceMessageID("missing-sender-headerless").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(headerless,
		[]byte("Subject: No sender\r\n\r\nBody\r\n")),
		"UpsertMessageRaw headerless")

	chat := f.NewMessage().WithSourceMessageID("missing-sender-chat").Build()
	chat.MessageType = store.MessageTypeGoogleChat
	chatID, err := f.Store.UpsertMessage(chat)
	require.NoError(err, "UpsertMessage chat")
	require.NoError(f.Store.UpsertMessageRaw(chatID, malformedRaw),
		"UpsertMessageRaw chat")

	existingSender := f.EnsureParticipant(
		"existing@example.test", "Existing", "example.test",
	)
	withSender := f.NewMessage().WithSourceMessageID("existing-sender").Build()
	withSender.SenderID = sql.NullInt64{Int64: existingSender, Valid: true}
	withSenderID, err := f.Store.UpsertMessage(withSender)
	require.NoError(err, "UpsertMessage existing sender")
	require.NoError(f.Store.UpsertMessageRaw(withSenderID, malformedRaw),
		"UpsertMessageRaw existing sender")

	existingFrom := f.NewMessage().
		WithSourceMessageID("existing-from").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(existingFrom, malformedRaw),
		"UpsertMessageRaw existing from")
	require.NoError(f.Store.ReplaceMessageRecipients(
		existingFrom, "from", []int64{existingSender}, []string{"Existing"},
	), "ReplaceMessageRecipients existing from")

	candidates, err := f.Store.ListMissingMIMESendersPageContext(t.Context(), 0, 1)
	require.NoError(err, "ListMissingMIMESendersPageContext first page")
	require.Len(candidates, 1, "page size limits retained raw MIME")
	assert.Equal(repairable, candidates[0].MessageID)
	headerEnd := bytes.Index(malformedRaw, []byte("\r\n\r\n")) + len("\r\n\r\n")
	assert.Equal(malformedRaw[:headerEnd], candidates[0].RawMIME,
		"sender scan should retain only the bounded MIME header")
	assert.Equal(f.Source.ID, candidates[0].SourceID)
	assert.Equal("gmail", candidates[0].SourceType)

	secondPage, err := f.Store.ListMissingMIMESendersPageContext(
		t.Context(), candidates[0].MessageID, 1,
	)
	require.NoError(err, "ListMissingMIMESendersPageContext second page")
	require.Len(secondPage, 1)
	assert.Equal(headerless, secondPage[0].MessageID)

	parsed, err := mime.ParseWithRecovery(candidates[0].RawMIME, "")
	require.NoError(err, "ParseWithRecovery malformed From")
	require.Len(parsed.From, 1, "salvaged From address")
	revisionBefore, err := f.Store.DerivedDataRevision()
	require.NoError(err, "DerivedDataRevision before repair")
	require.NoError(f.Store.ApplySenderRepairContext(
		t.Context(), repairable, candidates[0].RawMIMEFingerprint, parsed.From,
	), "ApplySenderRepairContext")
	revisionAfter, err := f.Store.DerivedDataRevision()
	require.NoError(err, "DerivedDataRevision after repair")
	assert.Equal(revisionBefore+1, revisionAfter,
		"sender repair must invalidate exported message facts")

	var senderID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT sender_id FROM messages WHERE id = ?
	`), repairable).Scan(&senderID), "read repaired sender_id")
	var email, displayName string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT p.email_address, COALESCE(mr.display_name, '')
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'from'
	`), repairable).Scan(&email, &displayName), "read repaired from recipient")
	assert.Equal("noreply@fridgeco.example", email)
	assert.Equal(senderID, f.GetSingleRecipientID(repairable, "from"))
	assert.Empty(displayName,
		"fallback parser must not invent a display name from malformed syntax")
	f.AssertRecipientCount(repairable, "from", 1)

	candidates, err = f.Store.ListMissingMIMESendersPageContext(t.Context(), 0, 100)
	require.NoError(err, "ListMissingMIMESendersPageContext after repair")
	require.Len(candidates, 1, "only the truly headerless MIME remains")
	assert.Equal(headerless, candidates[0].MessageID)
}

func TestApplySenderRepairRejectsChangedCandidate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.NewMessage().
		WithSourceMessageID("sender-repair-stale").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("From: stale@example.test\r\n\r\nBody\r\n")),
		"UpsertMessageRaw stale candidate")
	candidate := senderRepairCandidateForMessage(t, f.Store, messageID)
	participantID := f.EnsureParticipant(
		"newer@example.test", "Newer Sender", "example.test",
	)
	require.NoError(f.Store.ReplaceMessageRecipients(
		messageID, "from", []int64{participantID}, []string{"Newer Sender"},
	), "write newer from snapshot")
	revisionBefore, err := f.Store.DerivedDataRevision()
	require.NoError(err, "DerivedDataRevision before stale repair")

	err = f.Store.ApplySenderRepairContext(
		t.Context(), messageID, candidate.RawMIMEFingerprint, []mime.Address{{
			Email: "stale@example.test", Domain: "example.test",
		}})
	require.ErrorContains(err, "changed after sender repair planning")
	revisionAfter, revisionErr := f.Store.DerivedDataRevision()
	require.NoError(revisionErr, "DerivedDataRevision after stale repair")
	assert.Equal(revisionBefore, revisionAfter,
		"rejected repair must not invalidate derived data")

	var senderID sql.NullInt64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`), messageID).Scan(&senderID))
	assert.False(senderID.Valid, "stale repair must not set sender_id")
	assert.Equal(participantID, f.GetSingleRecipientID(messageID, "from"),
		"stale repair must preserve the newer from snapshot")
}

func TestApplySenderRepairRejectsChangedRawMIME(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.NewMessage().
		WithSourceMessageID("sender-repair-stale-raw").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("From: planned@example.test\r\n\r\nBody\r\n")),
		"store planned raw MIME")
	candidates, err := f.Store.ListMissingMIMESendersPageContext(t.Context(), 0, 100)
	require.NoError(err)
	require.Len(candidates, 1)
	planned, err := mime.ParseWithRecovery(candidates[0].RawMIME, "")
	require.NoError(err)
	require.Len(planned.From, 1)

	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("From: changed@example.test\r\n\r\nBody\r\n")),
		"replace raw MIME after planning")
	err = f.Store.ApplySenderRepairContext(
		t.Context(), messageID, candidates[0].RawMIMEFingerprint, planned.From,
	)
	require.ErrorContains(err, "changed after sender repair planning")
	var senderID sql.NullInt64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`), messageID).Scan(&senderID))
	assert.False(t, senderID.Valid, "stale MIME evidence must not write a sender")
}

func TestApplySenderRepairSerializesConcurrentFromWriter(t *testing.T) {
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "sender-repair-lock.db")
	repairStore, err := store.OpenForTest(dbPath)
	require.NoError(err, "open repair store")
	t.Cleanup(func() { _ = repairStore.Close() })
	require.NoError(repairStore.InitSchema(), "init repair store")
	writerStore, err := store.OpenForTest(dbPath)
	require.NoError(err, "open concurrent writer store")
	t.Cleanup(func() { _ = writerStore.Close() })

	source, err := repairStore.GetOrCreateSource("gmail", "sender-lock@example.test")
	require.NoError(err)
	conversationID, err := repairStore.EnsureConversation(
		source.ID, "sender-lock-thread", "Sender lock thread",
	)
	require.NoError(err)
	messageID, err := repairStore.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "sender-lock-message", MessageType: store.MessageTypeEmail,
	})
	require.NoError(err)
	require.NoError(repairStore.UpsertMessageRaw(messageID,
		[]byte("From: repaired@example.test\r\n\r\nBody\r\n")))
	candidate := senderRepairCandidateForMessage(t, repairStore, messageID)
	newerParticipant, err := writerStore.EnsureParticipant(
		"newer@example.test", "Newer Sender", "example.test",
	)
	require.NoError(err)

	locked := make(chan struct{})
	release := make(chan struct{})
	repairStore.SetSenderRepairMessageLockHookForTest(func() {
		close(locked)
		<-release
	})
	repairDone := make(chan error, 1)
	go func() {
		repairDone <- repairStore.ApplySenderRepairContext(
			t.Context(), messageID, candidate.RawMIMEFingerprint, []mime.Address{{
				Email: "repaired@example.test", Domain: "example.test",
			}},
		)
	}()
	<-locked

	writerDone := make(chan error, 1)
	go func() {
		writerDone <- writerStore.ReplaceMessageRecipients(
			messageID, "from", []int64{newerParticipant}, []string{"Newer Sender"},
		)
	}()
	select {
	case writerErr := <-writerDone:
		close(release)
		<-repairDone
		require.FailNow("concurrent From writer bypassed sender-repair lock",
			"ReplaceMessageRecipients returned early: %v", writerErr)
	case <-time.After(time.Second):
	}

	close(release)
	require.NoError(<-repairDone, "sender repair")
	require.NoError(<-writerDone, "later From replacement")
	var finalParticipant int64
	require.NoError(repairStore.DB().QueryRow(repairStore.Rebind(`
		SELECT participant_id FROM message_recipients
		WHERE message_id = ? AND recipient_type = 'from'
	`), messageID).Scan(&finalParticipant))
	assert.Equal(t, newerParticipant, finalParticipant, "later writer must win")
}

func TestApplySenderRepairRefreshesSQLiteFTS(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("directly verifies the standalone SQLite FTS5 row")
	}
	if !f.Store.FTS5Available() {
		t.Skip("FTS5 not available")
	}
	messageID := f.NewMessage().
		WithSourceMessageID("sender-repair-fts").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("From: repairftsneedle@example.test\r\n\r\nBody\r\n")),
		"UpsertMessageRaw repair candidate")
	require.NoError(f.Store.UpsertMessageBody(messageID,
		sql.NullString{String: "preservedbodyneedle", Valid: true}, sql.NullString{}),
		"UpsertMessageBody repair candidate")
	require.NoError(f.Store.UpsertFTS(
		messageID, "Repair subject", "preservedbodyneedle", "", "", "",
	), "seed FTS without sender")
	candidate := senderRepairCandidateForMessage(t, f.Store, messageID)

	require.NoError(f.Store.ApplySenderRepairContext(
		t.Context(), messageID, candidate.RawMIMEFingerprint, []mime.Address{{
			Email: "repairftsneedle@example.test", Domain: "example.test",
		}}), "ApplySenderRepairContext")

	for term, want := range map[string]int{
		"repairftsneedle":     1,
		"preservedbodyneedle": 1,
	} {
		var got int
		require.NoError(f.Store.DB().QueryRow(
			`SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, term,
		).Scan(&got), "search refreshed FTS for %s", term)
		assert.Equal(t, want, got, "FTS match count for %s", term)
	}
}

func TestApplySenderRepairPersistsEveryRecoveredFromAddress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	messageID := f.NewMessage().
		WithSourceMessageID("sender-repair-multi-from").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("From: First <first@example.test>, second@example.test\r\n"+
			"Subject: Two senders\r\n\r\nBody\r\n")),
		"UpsertMessageRaw multi-From candidate")
	candidate := senderRepairCandidateForMessage(t, f.Store, messageID)
	parsed, err := mime.ParseWithRecovery(candidate.RawMIME, "")
	require.NoError(err, "parse multi-From header")
	require.Len(parsed.From, 2, "both From addresses recovered")

	require.NoError(f.Store.ApplySenderRepairContext(
		t.Context(), messageID, candidate.RawMIMEFingerprint, parsed.From,
	), "ApplySenderRepairContext")

	var senderEmail string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT p.email_address
		FROM messages m JOIN participants p ON p.id = m.sender_id
		WHERE m.id = ?
	`), messageID).Scan(&senderEmail), "read repaired sender")
	assert.Equal("first@example.test", senderEmail,
		"sender_id must point at the first recovered address")

	rows, err := f.Store.DB().Query(f.Store.Rebind(`
		SELECT p.email_address
		FROM message_recipients mr
		JOIN participants p ON p.id = mr.participant_id
		WHERE mr.message_id = ? AND mr.recipient_type = 'from'
		ORDER BY p.email_address
	`), messageID)
	require.NoError(err, "list repaired From rows")
	defer func() { _ = rows.Close() }()
	var fromEmails []string
	for rows.Next() {
		var email string
		require.NoError(rows.Scan(&email), "scan From row")
		fromEmails = append(fromEmails, email)
	}
	require.NoError(rows.Err(), "iterate From rows")
	assert.Equal([]string{"first@example.test", "second@example.test"}, fromEmails,
		"the envelope snapshot must keep every recovered From address")
}

func TestMissingMIMESenderScanIncludesLegacyNullMessageType(t *testing.T) {
	require := require.New(t)
	st, sourceID, conversationID := newLegacyNullableMessageTypeStore(t)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conversationID,
		SourceMessageID: "sender-repair-null-type-scan", MessageType: store.MessageTypeEmail,
	})
	require.NoError(err, "create NULL-type scan candidate")
	_, err = st.DB().Exec(st.Rebind(
		`UPDATE messages SET message_type = NULL WHERE id = ?`), messageID)
	require.NoError(err, "clear legacy message type")
	require.NoError(st.UpsertMessageRaw(messageID,
		[]byte("From: nullscan@example.test\r\n\r\nBody\r\n")),
		"UpsertMessageRaw NULL-type candidate")

	candidates, err := st.ListMissingMIMESendersPageContext(t.Context(), 0, 100)
	require.NoError(err)
	require.Len(candidates, 1, "legacy NULL email must be repairable")
	assert.Equal(t, messageID, candidates[0].MessageID)
}

func TestApplySenderRepairAcceptsLegacyNullMessageType(t *testing.T) {
	require := require.New(t)
	st, sourceID, conversationID := newLegacyNullableMessageTypeStore(t)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conversationID,
		SourceMessageID: "sender-repair-null-type-apply", MessageType: store.MessageTypeEmail,
	})
	require.NoError(err, "create NULL-type apply candidate")
	_, err = st.DB().Exec(st.Rebind(
		`UPDATE messages SET message_type = NULL WHERE id = ?`), messageID)
	require.NoError(err, "clear legacy message type")
	require.NoError(st.UpsertMessageRaw(messageID,
		[]byte("From: nullapply@example.test\r\n\r\nBody\r\n")),
		"UpsertMessageRaw NULL-type candidate")
	candidate := senderRepairCandidateForMessage(t, st, messageID)

	require.NoError(st.ApplySenderRepairContext(
		t.Context(), messageID, candidate.RawMIMEFingerprint, []mime.Address{{
			Email: "nullapply@example.test", Domain: "example.test",
		}}), "ApplySenderRepairContext")
	var senderID sql.NullInt64
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`), messageID).Scan(&senderID))
	assert.True(t, senderID.Valid, "legacy NULL email must receive its repaired sender")
}

func newLegacyNullableMessageTypeStore(t *testing.T) (*store.Store, int64, int64) {
	t.Helper()
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "legacy-null-message-type.db")
	seed, err := store.OpenForTest(dbPath)
	require.NoError(err, "open seed store")
	require.NoError(seed.InitSchema(), "init seed schema")

	ctx := context.Background()
	conn, err := seed.DB().Conn(ctx)
	require.NoError(err, "pin schema rewrite connection")
	var schema string
	require.NoError(conn.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'messages'`,
	).Scan(&schema), "read messages schema")
	rewritten := strings.Replace(schema,
		"message_type TEXT NOT NULL", "message_type TEXT", 1)
	require.NotEqual(schema, rewritten, "remove current NOT NULL for legacy fixture")
	require.NoError(func() error {
		if _, err := conn.ExecContext(ctx, `PRAGMA writable_schema=ON`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE sqlite_master SET sql = ? WHERE type = 'table' AND name = 'messages'`,
			rewritten,
		); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `PRAGMA writable_schema=OFF`)
		return err
	}(), "rewrite legacy messages schema")
	require.NoError(conn.Close(), "close schema rewrite connection")
	require.NoError(seed.Close(), "close seed store")

	st, err := store.OpenForTest(dbPath)
	require.NoError(err, "reopen legacy store")
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema(), "init legacy store")
	source, err := st.GetOrCreateSource("gmail", "legacy-null@example.test")
	require.NoError(err, "create legacy source")
	conversationID, err := st.EnsureConversation(
		source.ID, "legacy-null-thread", "Legacy NULL thread",
	)
	require.NoError(err, "create legacy conversation")
	return st, source.ID, conversationID
}

func TestMissingMIMESenderScanSkipsOversizedHeaderWithoutBlockingLaterRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	oversized := f.NewMessage().
		WithSourceMessageID("missing-sender-oversized-header").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(oversized,
		[]byte("X-Oversized: "+strings.Repeat("x", 300<<10))),
		"UpsertMessageRaw oversized header")
	valid := f.NewMessage().
		WithSourceMessageID("missing-sender-after-oversized").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(valid,
		[]byte("From: later@example.test\r\n\r\nBody")),
		"UpsertMessageRaw later valid header")

	candidates, err := f.Store.ListMissingMIMESendersPageContext(t.Context(), 0, 100)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.Equal(oversized, candidates[0].MessageID)
	assert.Empty(candidates[0].RawMIME,
		"oversized header remains unresolved without retaining its payload")
	assert.Equal(valid, candidates[1].MessageID)
	assert.Equal([]byte("From: later@example.test\r\n\r\n"), candidates[1].RawMIME)
}

func TestMissingMIMESenderScanSkipsCorruptCompressionWithoutBlockingLaterRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	corrupt := f.NewMessage().
		WithSourceMessageID("missing-sender-corrupt-compression").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(corrupt,
		[]byte("From: corrupt@example.test\r\n\r\nBody")))
	_, err := f.Store.DB().Exec(f.Store.Rebind(`
		UPDATE message_raw SET raw_data = ?, compression = 'zlib'
		WHERE message_id = ? AND raw_format = 'mime'
	`), []byte("not a zlib stream"), corrupt)
	require.NoError(err)

	valid := f.NewMessage().
		WithSourceMessageID("missing-sender-after-corrupt-compression").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(valid,
		[]byte("From: later@example.test\r\n\r\nBody")))

	candidates, err := f.Store.ListMissingMIMESendersPageContext(t.Context(), 0, 100)
	require.NoError(err)
	require.Len(candidates, 2)
	assert.Equal(corrupt, candidates[0].MessageID)
	require.Error(candidates[0].DecodeError)
	assert.Empty(candidates[0].RawMIME)
	assert.Equal(valid, candidates[1].MessageID)
	assert.Equal([]byte("From: later@example.test\r\n\r\n"), candidates[1].RawMIME)
}

func TestApplySenderRepairRollsBackSenderWhenRecipientWriteFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	if f.Store.IsPostgreSQL() {
		t.Skip("SQLite trigger injection; the shared transaction path is backend-neutral")
	}
	messageID := f.NewMessage().
		WithSourceMessageID("sender-repair-rollback").
		Create(t, f.Store)
	require.NoError(f.Store.UpsertMessageRaw(messageID,
		[]byte("From: rollback@example.test\r\n\r\nBody\r\n")),
		"UpsertMessageRaw rollback candidate")
	candidate := senderRepairCandidateForMessage(t, f.Store, messageID)
	_, err := f.Store.DB().Exec(`
		CREATE TRIGGER fail_sender_repair_recipient
		BEFORE INSERT ON message_recipients
		WHEN NEW.message_id = ` + sqlLiteralInt64(messageID) + `
		BEGIN
			SELECT RAISE(ABORT, 'synthetic sender recipient failure');
		END
	`)
	require.NoError(err, "create failure trigger")
	revisionBefore, err := f.Store.DerivedDataRevision()
	require.NoError(err, "DerivedDataRevision before rolled-back repair")

	err = f.Store.ApplySenderRepairContext(
		t.Context(), messageID, candidate.RawMIMEFingerprint, []mime.Address{{
			Email: "rollback@example.test", Domain: "example.test",
		}})
	require.ErrorContains(err, "synthetic sender recipient failure")
	revisionAfter, revisionErr := f.Store.DerivedDataRevision()
	require.NoError(revisionErr, "DerivedDataRevision after rolled-back repair")
	assert.Equal(revisionBefore, revisionAfter,
		"rolled-back repair must not invalidate derived data")

	var senderID sql.NullInt64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT sender_id FROM messages WHERE id = ?`), messageID).Scan(&senderID))
	assert.False(senderID.Valid, "sender_id update must roll back")
	f.AssertRecipientCount(messageID, "from", 0)
}

func sqlLiteralInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

func senderRepairCandidateForMessage(
	t *testing.T,
	st *store.Store,
	messageID int64,
) store.MissingSenderCandidate {
	t.Helper()
	candidates, err := st.ListMissingMIMESendersPageContext(t.Context(), 0, 100)
	require.NoError(t, err, "list sender repair candidates")
	for _, candidate := range candidates {
		if candidate.MessageID == messageID {
			return candidate
		}
	}
	require.FailNow(t, "sender repair candidate not found", "message ID %d", messageID)
	return store.MissingSenderCandidate{}
}
