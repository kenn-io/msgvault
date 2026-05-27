package query

import (
	"context"
	"database/sql"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestDuckDBEngine_QuerySQL(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	builder := NewTestDataBuilder(t)
	srcID := builder.AddSource("test@example.com")
	bob := builder.AddParticipant(
		"bob@example.com", "example.com", "Bob",
	)
	msgID := builder.AddMessage(MessageOpt{
		Subject: "Test", SourceID: srcID, SizeEstimate: 100,
	})
	builder.AddFrom(msgID, bob, "Bob")

	engine := builder.BuildEngine()
	defer func() { _ = engine.Close() }()

	ctx := context.Background()
	result, err := engine.QuerySQL(ctx,
		"SELECT from_email, message_count FROM v_senders")
	require.NoError(err, "QuerySQL")
	require.GreaterOrEqual(len(result.Columns), 2, "columns")
	assert.Equal("from_email", result.Columns[0])
	assert.Equal(1, result.RowCount)
}

func TestDuckDBEngine_QuerySQL_Error(t *testing.T) {
	builder := NewTestDataBuilder(t)
	builder.AddSource("test@example.com")
	engine := builder.BuildEngine()
	defer func() { _ = engine.Close() }()

	_, err := engine.QuerySQL(
		context.Background(),
		"SELECT * FROM nonexistent_table",
	)
	requirepkg.Error(t, err, "expected error for bad SQL")
}

func TestRegisterViews_BaseViews(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	builder := NewTestDataBuilder(t)
	srcID := builder.AddSource("alice@example.com")
	partID := builder.AddParticipant(
		"bob@example.com", "example.com", "Bob",
	)
	lblID := builder.AddLabel("INBOX")
	msgID := builder.AddMessage(MessageOpt{
		Subject:  "Hello",
		SourceID: srcID,
	})
	builder.AddFrom(msgID, partID, "Bob")
	builder.AddMessageLabel(msgID, lblID)

	dir, cleanup := builder.Build()
	defer cleanup()

	engine := builder.BuildEngine()
	defer func() { _ = engine.Close() }()

	require.NoError(RegisterViews(engine.db, dir), "RegisterViews")

	tables := []string{
		"messages", "participants", "message_recipients",
		"labels", "message_labels", "attachments",
		"conversations", "sources",
	}
	for _, table := range tables {
		var count int
		err := engine.db.QueryRowContext(
			context.Background(),
			"SELECT COUNT(*) FROM "+table,
		).Scan(&count)
		require.NoError(err, "query %s", table)
	}

	var id int64
	var subject, messageType string
	var attachmentCount int
	err := engine.db.QueryRowContext(
		context.Background(),
		"SELECT id, subject, attachment_count, message_type FROM messages LIMIT 1",
	).Scan(&id, &subject, &attachmentCount, &messageType)
	require.NoError(err, "scan messages")
	assert.Equal("Hello", subject)
}

func TestRegisterViews_ConvenienceViews(t *testing.T) {
	builder := NewTestDataBuilder(t)
	srcID := builder.AddSource("alice@example.com")
	bob := builder.AddParticipant(
		"bob@corp.com", "corp.com", "Bob Smith",
	)
	carol := builder.AddParticipant(
		"carol@corp.com", "corp.com", "Carol",
	)
	inbox := builder.AddLabel("INBOX")
	sent := builder.AddLabel("SENT")

	msg1 := builder.AddMessage(MessageOpt{
		Subject:      "First",
		SourceID:     srcID,
		SizeEstimate: 1000,
	})
	builder.AddFrom(msg1, bob, "Bob Smith")
	builder.AddTo(msg1, carol, "Carol")
	builder.AddMessageLabel(msg1, inbox)
	builder.AddAttachment(msg1, 500, "doc.pdf")

	msg2 := builder.AddMessage(MessageOpt{
		Subject:      "Second",
		SourceID:     srcID,
		SizeEstimate: 2000,
	})
	builder.AddFrom(msg2, bob, "Bob Smith")
	builder.AddMessageLabel(msg2, inbox)
	builder.AddMessageLabel(msg2, sent)

	dir, cleanup := builder.Build()
	defer cleanup()
	engine := builder.BuildEngine()
	defer func() { _ = engine.Close() }()

	requirepkg.NoError(t, RegisterViews(engine.db, dir), "RegisterViews")
	ctx := context.Background()

	t.Run("v_messages", func(t *testing.T) {
		assert := assertpkg.New(t)
		var fromEmail, fromDomain, labels string
		err := engine.db.QueryRowContext(ctx,
			"SELECT from_email, from_domain, labels "+
				"FROM v_messages WHERE subject = 'First'",
		).Scan(&fromEmail, &fromDomain, &labels)
		requirepkg.NoError(t, err, "scan v_messages")
		assert.Equal("bob@corp.com", fromEmail)
		assert.Equal("corp.com", fromDomain)
		assert.Equal(`["INBOX"]`, labels)
	})

	t.Run("v_messages_multi_labels", func(t *testing.T) {
		var labels string
		err := engine.db.QueryRowContext(ctx,
			"SELECT labels FROM v_messages "+
				"WHERE subject = 'Second'",
		).Scan(&labels)
		requirepkg.NoError(t, err, "scan v_messages")
		assertpkg.Equal(t, `["INBOX","SENT"]`, labels)
	})

	t.Run("v_senders", func(t *testing.T) {
		assert := assertpkg.New(t)
		var fromName string
		var msgCount int64
		var totalSize int64
		err := engine.db.QueryRowContext(ctx,
			"SELECT from_name, message_count, total_size "+
				"FROM v_senders "+
				"WHERE from_email = 'bob@corp.com'",
		).Scan(&fromName, &msgCount, &totalSize)
		requirepkg.NoError(t, err, "scan v_senders")
		assert.Equal("Bob Smith", fromName)
		assert.Equal(int64(2), msgCount)
		assert.Equal(int64(3000), totalSize)
	})

	t.Run("v_domains", func(t *testing.T) {
		var msgCount, senderCount int64
		err := engine.db.QueryRowContext(ctx,
			"SELECT message_count, sender_count "+
				"FROM v_domains "+
				"WHERE domain = 'corp.com'",
		).Scan(&msgCount, &senderCount)
		requirepkg.NoError(t, err, "scan v_domains")
		assertpkg.Equal(t, int64(2), msgCount)
		assertpkg.Equal(t, int64(1), senderCount)
	})

	t.Run("v_labels", func(t *testing.T) {
		var msgCount int64
		err := engine.db.QueryRowContext(ctx,
			"SELECT message_count FROM v_labels "+
				"WHERE name = 'INBOX'",
		).Scan(&msgCount)
		requirepkg.NoError(t, err, "scan v_labels")
		assertpkg.Equal(t, int64(2), msgCount)
	})

	t.Run("v_threads", func(t *testing.T) {
		require := requirepkg.New(t)
		assert := assertpkg.New(t)
		// Both messages share the same conversation (auto-assigned),
		// so we expect exactly 2 threads (one per conversation).
		var threadCount int
		err := engine.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM v_threads",
		).Scan(&threadCount)
		require.NoError(err, "scan v_threads count")
		assert.Equal(2, threadCount)

		// Sum of message_count across all threads should be 2.
		var totalMsgCount int64
		err = engine.db.QueryRowContext(ctx,
			"SELECT SUM(message_count) FROM v_threads",
		).Scan(&totalMsgCount)
		require.NoError(err, "scan v_threads sum")
		assert.Equal(int64(2), totalMsgCount)

		// Verify participant_emails, conversation_title, conversation_type
		var participantEmails sql.NullString
		var convTitle, convType string
		err = engine.db.QueryRowContext(ctx,
			"SELECT participant_emails, conversation_title, conversation_type FROM v_threads LIMIT 1",
		).Scan(&participantEmails, &convTitle, &convType)
		require.NoError(err, "scan v_threads columns")
		assert.Equal("email", convType)
	})
}
