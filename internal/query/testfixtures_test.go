package query

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/marcboeker/go-duckdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Typed fixture structs
// ---------------------------------------------------------------------------

// MessageFixture defines a message row for Parquet test data.
type MessageFixture struct {
	ID              int64
	SourceID        int64
	SourceMessageID string
	ConversationID  int64
	Subject         string
	Snippet         string
	SentAt          time.Time
	SizeEstimate    int64
	HasAttachments  bool
	AttachmentCount int
	DeletedAt       *time.Time // nil = NULL
	SenderID        *int64     // nil = NULL (direct sender for WhatsApp/chat messages)
	MessageType     string     // e.g. "email", "whatsapp"
	Year            int
	Month           int
}

// SourceFixture defines a source row for Parquet test data.
type SourceFixture struct {
	ID           int64
	AccountEmail string
	SourceType   string // "gmail", "whatsapp", etc. Defaults to "gmail".
}

// ParticipantFixture defines a participant row for Parquet test data.
type ParticipantFixture struct {
	ID          int64
	Email       string
	Domain      string
	DisplayName string
	PhoneNumber string // E.164 phone number (for WhatsApp/chat participants)
}

// RecipientFixture defines a message_recipients row for Parquet test data.
type RecipientFixture struct {
	MessageID     int64
	ParticipantID int64
	Type          string // "from", "to", "cc", "bcc"
	DisplayName   string
}

// LabelFixture defines a label row for Parquet test data.
type LabelFixture struct {
	ID   int64
	Name string
}

// MessageLabelFixture defines a message_labels row for Parquet test data.
type MessageLabelFixture struct {
	MessageID int64
	LabelID   int64
}

// AttachmentFixture defines an attachment row for Parquet test data.
type AttachmentFixture struct {
	MessageID int64
	Size      int64
	Filename  string
}

// ConversationFixture defines a conversation row for Parquet test data.
type ConversationFixture struct {
	ID                   int64
	SourceConversationID string
	Title                string // Group/chat name (for WhatsApp/chat conversations)
}

// ---------------------------------------------------------------------------
// TestDataBuilder: typed builder that generates Parquet test data
// ---------------------------------------------------------------------------

// TestDataBuilder accumulates typed fixture data and generates Parquet files.
type TestDataBuilder struct {
	t           testing.TB
	nextMsgID   int64
	nextSrcID   int64
	nextPartID  int64
	nextLabelID int64
	nextConvID  int64

	sources       []SourceFixture
	messages      []MessageFixture
	participants  []ParticipantFixture
	recipients    []RecipientFixture
	labels        []LabelFixture
	msgLabels     []MessageLabelFixture
	attachments   []AttachmentFixture
	conversations []ConversationFixture

	emptyAttachments bool // if true, write empty attachments file
}

// NewTestDataBuilder creates a new typed test data builder.
func NewTestDataBuilder(tb testing.TB) *TestDataBuilder {
	tb.Helper()
	return &TestDataBuilder{
		t:           tb,
		nextMsgID:   1,
		nextSrcID:   1,
		nextPartID:  1,
		nextLabelID: 1,
		nextConvID:  200,
	}
}

// AddSource adds a source and returns its ID.
func (b *TestDataBuilder) AddSource(email string) int64 {
	return b.AddSourceWithType(email, "gmail")
}

// AddSourceWithType adds a source with a specific type and returns its ID.
func (b *TestDataBuilder) AddSourceWithType(email, sourceType string) int64 {
	id := b.nextSrcID
	b.nextSrcID++
	b.sources = append(b.sources, SourceFixture{ID: id, AccountEmail: email, SourceType: sourceType})
	return id
}

// AddParticipant adds a participant and returns its ID.
func (b *TestDataBuilder) AddParticipant(email, domain, displayName string) int64 {
	id := b.nextPartID
	b.nextPartID++
	b.participants = append(b.participants, ParticipantFixture{
		ID: id, Email: email, Domain: domain, DisplayName: displayName,
	})
	return id
}

// AddPhoneParticipant adds a phone-only participant (no email/domain) and
// returns its ID. Mirrors the iMessage/SMS shape: phone_number set,
// email_address NULL/empty.
func (b *TestDataBuilder) AddPhoneParticipant(phone, displayName string) int64 {
	id := b.nextPartID
	b.nextPartID++
	b.participants = append(b.participants, ParticipantFixture{
		ID: id, DisplayName: displayName, PhoneNumber: phone,
	})
	return id
}

// AddLabel adds a label and returns its ID. Name must be non-empty.
func (b *TestDataBuilder) AddLabel(name string) int64 {
	b.t.Helper()
	require.NotEmpty(b.t, name, "AddLabel: name is required")
	id := b.nextLabelID
	b.nextLabelID++
	b.labels = append(b.labels, LabelFixture{ID: id, Name: name})
	return id
}

// MessageOpt configures a message to add.
type MessageOpt struct {
	Subject        string
	Snippet        string
	SentAt         time.Time
	SizeEstimate   int64
	HasAttachments bool
	DeletedAt      *time.Time
	SourceID       int64  // defaults to 1
	ConversationID int64  // 0 = auto-assign
	MessageType    string // defaults to "email"
	SenderID       *int64 // nil = NULL (direct sender for text/chat messages)
}

// AddMessage adds a message and returns its ID.
func (b *TestDataBuilder) AddMessage(opt MessageOpt) int64 {
	id := b.nextMsgID
	b.nextMsgID++

	srcID := opt.SourceID
	if srcID == 0 {
		require.NotEmpty(b.t, b.sources, "AddMessage: no sources added; call AddSource before AddMessage or set SourceID explicitly")
		srcID = b.sources[0].ID
	}
	convID := opt.ConversationID
	if convID == 0 {
		convID = b.nextConvID
		b.nextConvID++
	}
	sentAt := opt.SentAt
	if sentAt.IsZero() {
		sentAt = time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	}
	snippet := opt.Snippet
	if snippet == "" {
		snippet = fmt.Sprintf("Preview %d", id)
	}

	b.messages = append(b.messages, MessageFixture{
		ID:              id,
		SourceID:        srcID,
		SourceMessageID: fmt.Sprintf("msg%d", id),
		ConversationID:  convID,
		Subject:         opt.Subject,
		Snippet:         snippet,
		SentAt:          sentAt,
		SizeEstimate:    opt.SizeEstimate,
		HasAttachments:  opt.HasAttachments,
		DeletedAt:       opt.DeletedAt,
		SenderID:        opt.SenderID,
		MessageType:     opt.MessageType,
		Year:            sentAt.Year(),
		Month:           int(sentAt.Month()),
	})

	// Track conversation if not already present
	convExists := false
	for _, c := range b.conversations {
		if c.ID == convID {
			convExists = true
			break
		}
	}
	if !convExists {
		b.conversations = append(b.conversations, ConversationFixture{
			ID:                   convID,
			SourceConversationID: fmt.Sprintf("thread%d", convID),
		})
	}

	return id
}

// AddRecipient adds a message_recipients row.
func (b *TestDataBuilder) AddRecipient(messageID, participantID int64, recipientType, displayName string) {
	b.recipients = append(b.recipients, RecipientFixture{
		MessageID: messageID, ParticipantID: participantID,
		Type: recipientType, DisplayName: displayName,
	})
}

// AddFrom is shorthand for AddRecipient with type "from".
func (b *TestDataBuilder) AddFrom(messageID, participantID int64, displayName string) {
	b.AddRecipient(messageID, participantID, "from", displayName)
}

// AddTo is shorthand for AddRecipient with type "to".
func (b *TestDataBuilder) AddTo(messageID, participantID int64, displayName string) {
	b.AddRecipient(messageID, participantID, "to", displayName)
}

// AddCc is shorthand for AddRecipient with type "cc".
func (b *TestDataBuilder) AddCc(messageID, participantID int64, displayName string) {
	b.AddRecipient(messageID, participantID, "cc", displayName)
}

// AddMessageLabel associates a message with a label.
func (b *TestDataBuilder) AddMessageLabel(messageID, labelID int64) {
	b.msgLabels = append(b.msgLabels, MessageLabelFixture{
		MessageID: messageID, LabelID: labelID,
	})
}

// AddAttachment adds an attachment row and sets HasAttachments on the related message.
func (b *TestDataBuilder) AddAttachment(messageID, size int64, filename string) {
	b.t.Helper()
	b.attachments = append(b.attachments, AttachmentFixture{
		MessageID: messageID, Size: size, Filename: filename,
	})
	// Ensure the related message has HasAttachments set to true.
	for i := range b.messages {
		if b.messages[i].ID == messageID {
			b.messages[i].HasAttachments = true
			return
		}
	}
	require.Failf(b.t, "AddAttachment: message not found",
		"message ID %d not found; add the message before attaching files", messageID)
}

// SetEmptyAttachments marks the attachments table as empty (schema only).
func (b *TestDataBuilder) SetEmptyAttachments() {
	b.emptyAttachments = true
}

// ---------------------------------------------------------------------------
// SQL generation
// ---------------------------------------------------------------------------

func sqlStr(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// joinRows maps each item to a SQL row string and joins them with commas.
func joinRows[T any](items []T, format func(T) string) string {
	rows := make([]string, len(items))
	for i, item := range items {
		rows[i] = format(item)
	}
	return strings.Join(rows, ",\n")
}

// toSQL converts a MessageFixture to a SQL VALUES row string.
func (m MessageFixture) toSQL() string {
	deletedAt := "NULL::TIMESTAMP"
	if m.DeletedAt != nil {
		deletedAt = fmt.Sprintf("TIMESTAMP '%s'", m.DeletedAt.Format("2006-01-02 15:04:05"))
	}
	senderID := "NULL::BIGINT"
	if m.SenderID != nil {
		senderID = fmt.Sprintf("%d::BIGINT", *m.SenderID)
	}
	msgType := m.MessageType
	if msgType == "" {
		msgType = "email"
	}
	return fmt.Sprintf("(%d::BIGINT, %d::BIGINT, %s, %d::BIGINT, %s, %s, TIMESTAMP '%s', %d::BIGINT, %v, %d, %s, %s, %s, %d, %d)",
		m.ID, m.SourceID, sqlStr(m.SourceMessageID), m.ConversationID,
		sqlStr(m.Subject), sqlStr(m.Snippet),
		m.SentAt.Format("2006-01-02 15:04:05"), m.SizeEstimate,
		m.HasAttachments, m.AttachmentCount, deletedAt, senderID, sqlStr(msgType), m.Year, m.Month,
	)
}

func (b *TestDataBuilder) sourcesSQL() string {
	return joinRows(b.sources, func(s SourceFixture) string {
		st := s.SourceType
		if st == "" {
			st = "gmail"
		}
		return fmt.Sprintf("(%d::BIGINT, %s, %s)", s.ID, sqlStr(s.AccountEmail), sqlStr(st))
	})
}

func (b *TestDataBuilder) participantsSQL() string {
	return joinRows(b.participants, func(p ParticipantFixture) string {
		return fmt.Sprintf("(%d::BIGINT, %s, %s, %s, %s)",
			p.ID, sqlStr(p.Email), sqlStr(p.Domain), sqlStr(p.DisplayName), sqlStr(p.PhoneNumber))
	})
}

func (b *TestDataBuilder) recipientsSQL() string {
	return joinRows(b.recipients, func(r RecipientFixture) string {
		return fmt.Sprintf("(%d::BIGINT, %d::BIGINT, %s, %s)",
			r.MessageID, r.ParticipantID, sqlStr(r.Type), sqlStr(r.DisplayName))
	})
}

func (b *TestDataBuilder) labelsSQL() string {
	return joinRows(b.labels, func(l LabelFixture) string {
		return fmt.Sprintf("(%d::BIGINT, %s)", l.ID, sqlStr(l.Name))
	})
}

func (b *TestDataBuilder) messageLabelsSQL() string {
	return joinRows(b.msgLabels, func(ml MessageLabelFixture) string {
		return fmt.Sprintf("(%d::BIGINT, %d::BIGINT)", ml.MessageID, ml.LabelID)
	})
}

func (b *TestDataBuilder) attachmentsSQL() string {
	return joinRows(b.attachments, func(a AttachmentFixture) string {
		return fmt.Sprintf("(%d::BIGINT, %d::BIGINT, %s)",
			a.MessageID, a.Size, sqlStr(a.Filename))
	})
}

func (b *TestDataBuilder) conversationsSQL() string {
	return joinRows(b.conversations, func(c ConversationFixture) string {
		return fmt.Sprintf("(%d::BIGINT, %s, %s)",
			c.ID, sqlStr(c.SourceConversationID), sqlStr(c.Title))
	})
}

// ---------------------------------------------------------------------------
// Build: generate Parquet files
// ---------------------------------------------------------------------------

// column definitions (coupled to SQL generation methods above).
const (
	messagesCols          = "id, source_id, source_message_id, conversation_id, subject, snippet, sent_at, size_estimate, has_attachments, attachment_count, deleted_from_source_at, sender_id, message_type, year, month"
	sourcesCols           = "id, account_email, source_type"
	participantsCols      = "id, email_address, domain, display_name, phone_number"
	messageRecipientsCols = "message_id, participant_id, recipient_type, display_name"
	labelsCols            = "id, name"
	messageLabelsCols     = "message_id, label_id"
	attachmentsCols       = "message_id, size, filename"
	conversationsCols     = "id, source_conversation_id, title"
)

// Build generates Parquet files from the accumulated data and returns the
// analytics directory path and a cleanup function.
func (b *TestDataBuilder) Build() (string, func()) {
	b.t.Helper()

	pb := newParquetBuilder(b.t)
	b.addMessageTables(pb)
	b.addAuxiliaryTables(pb)
	b.addAttachmentsTable(pb)

	return pb.build()
}

// addMessageTables partitions messages by year and adds each partition to the builder.
func (b *TestDataBuilder) addMessageTables(pb *parquetBuilder) {
	byYear := map[int][]MessageFixture{}
	for _, m := range b.messages {
		byYear[m.Year] = append(byYear[m.Year], m)
	}
	for year, msgs := range byYear {
		rows := joinRows(msgs, MessageFixture.toSQL)
		pb.addTable("messages",
			fmt.Sprintf("messages/year=%d", year), "data.parquet",
			messagesCols, rows)
	}
}

// addAuxiliaryTables adds sources, participants, recipients, labels, message_labels, and conversations.
func (b *TestDataBuilder) addAuxiliaryTables(pb *parquetBuilder) {
	auxTables := []struct {
		name, subdir, file, cols, dummy, sql string
		empty                                bool
	}{
		{"sources", "sources", "sources.parquet", sourcesCols, "(0::BIGINT, '', 'gmail')", b.sourcesSQL(), len(b.sources) == 0},
		{"participants", "participants", "participants.parquet", participantsCols, "(0::BIGINT, '', '', '', '')", b.participantsSQL(), len(b.participants) == 0},
		{"message_recipients", "message_recipients", "message_recipients.parquet", messageRecipientsCols, "(0::BIGINT, 0::BIGINT, '', '')", b.recipientsSQL(), len(b.recipients) == 0},
		{"labels", "labels", "labels.parquet", labelsCols, "(0::BIGINT, '')", b.labelsSQL(), len(b.labels) == 0},
		{"message_labels", "message_labels", "message_labels.parquet", messageLabelsCols, "(0::BIGINT, 0::BIGINT)", b.messageLabelsSQL(), len(b.msgLabels) == 0},
		{"conversations", "conversations", "conversations.parquet", conversationsCols, "(0::BIGINT, '', '')", b.conversationsSQL(), len(b.conversations) == 0},
	}
	for _, a := range auxTables {
		if a.empty {
			pb.addEmptyTable(a.name, a.subdir, a.file, a.cols, a.dummy)
		} else {
			pb.addTable(a.name, a.subdir, a.file, a.cols, a.sql)
		}
	}
}

// addAttachmentsTable adds the attachments table to the builder.
func (b *TestDataBuilder) addAttachmentsTable(pb *parquetBuilder) {
	if len(b.attachments) > 0 && !b.emptyAttachments {
		pb.addTable("attachments", "attachments", "attachments.parquet", attachmentsCols, b.attachmentsSQL())
	} else {
		pb.addEmptyTable("attachments", "attachments", "attachments.parquet", attachmentsCols,
			"(0::BIGINT, 0::BIGINT, '')")
	}
}

// BuildEngine generates Parquet files and returns a DuckDBEngine.
// Cleanup is registered via t.Cleanup.
func (b *TestDataBuilder) BuildEngine() *DuckDBEngine {
	b.t.Helper()
	analyticsDir, cleanup := b.Build()
	b.t.Cleanup(cleanup)
	engine, err := NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(b.t, err, "NewDuckDBEngine")
	b.t.Cleanup(func() { _ = engine.Close() })
	return engine
}

// ---------------------------------------------------------------------------
// Low-level Parquet builder (unchanged)
// ---------------------------------------------------------------------------

// parquetTable defines a table to be written as a Parquet file.
type parquetTable struct {
	name    string // e.g. "messages", "sources"
	subdir  string // subdirectory path relative to tmpDir, e.g. "messages/year=2024"
	file    string // filename, e.g. "data.parquet"
	columns string // column definition for the VALUES AS clause
	values  string // SQL VALUES rows (without the outer VALUES keyword)
	empty   bool   // if true, write schema-only empty file using WHERE false
}

// parquetBuilder creates a temp directory with Parquet test data files.
type parquetBuilder struct {
	t      testing.TB
	tables []parquetTable
}

// newParquetBuilder creates a new builder for Parquet test fixtures.
func newParquetBuilder(tb testing.TB) *parquetBuilder {
	tb.Helper()
	return &parquetBuilder{t: tb}
}

// addTable adds a table definition to be written as Parquet.
func (b *parquetBuilder) addTable(name, subdir, file, columns, values string) *parquetBuilder {
	b.tables = append(b.tables, parquetTable{
		name:    name,
		subdir:  subdir,
		file:    file,
		columns: columns,
		values:  values,
	})
	return b
}

// addEmptyTable adds an empty table (schema only, no rows) to be written as Parquet.
func (b *parquetBuilder) addEmptyTable(name, subdir, file, columns, dummyValues string) *parquetBuilder {
	b.tables = append(b.tables, parquetTable{
		name:    name,
		subdir:  subdir,
		file:    file,
		columns: columns,
		values:  dummyValues,
		empty:   true,
	})
	return b
}

// build creates the temp directory, writes all Parquet files, and returns the
// analytics directory path and a cleanup function.
func (b *parquetBuilder) build() (string, func()) {
	b.t.Helper()

	tmpDir := b.createTempDirs()

	db, err := sql.Open("duckdb", "")
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		require.NoError(b.t, err, "open duckdb")
	}
	defer func() { _ = db.Close() }()

	b.writeParquetFiles(db, tmpDir)

	return tmpDir, func() { _ = os.RemoveAll(tmpDir) }
}

// createTempDirs creates the temp directory and all required subdirectories.
func (b *parquetBuilder) createTempDirs() string {
	b.t.Helper()

	tmpDir, err := os.MkdirTemp("", "msgvault-test-parquet-*")
	require.NoError(b.t, err, "create temp dir")

	for _, tbl := range b.tables {
		dir := filepath.Join(tmpDir, tbl.subdir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			_ = os.RemoveAll(tmpDir)
			require.NoError(b.t, err, "create dir %s", dir)
		}
	}

	return tmpDir
}

// writeParquetFiles writes all table data to Parquet files.
func (b *parquetBuilder) writeParquetFiles(db *sql.DB, tmpDir string) {
	b.t.Helper()

	for _, tbl := range b.tables {
		path := escapePath(filepath.Join(tmpDir, tbl.subdir, tbl.file))
		writeTableParquet(b.t, db, path, tbl.columns, tbl.values, tbl.empty)
	}
}

// escapePath normalizes a file path for use in DuckDB SQL strings.
func escapePath(p string) string {
	return strings.ReplaceAll(filepath.ToSlash(p), "'", "''")
}

// writeTableParquet writes a single table's data to a Parquet file using DuckDB.
func writeTableParquet(tb testing.TB, db *sql.DB, path, columns, values string, empty bool) {
	tb.Helper()

	whereClause := ""
	if empty {
		whereClause = "\n\t\t\t\tWHERE false"
	}
	query := fmt.Sprintf(`
			COPY (
				SELECT * FROM (VALUES %s) AS t(%s)%s
			) TO '%s' (FORMAT PARQUET)
		`, values, columns, whereClause, path)

	_, err := db.Exec(query)
	require.NoError(tb, err, "create parquet %s", path)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// createEngineFromBuilder builds Parquet files from the builder and returns a
// DuckDBEngine. Cleanup is registered via t.Cleanup.
func createEngineFromBuilder(tb testing.TB, pb *parquetBuilder) *DuckDBEngine {
	tb.Helper()
	analyticsDir, cleanup := pb.build()
	tb.Cleanup(cleanup)
	engine, err := NewDuckDBEngine(analyticsDir, "", nil)
	require.NoError(tb, err, "NewDuckDBEngine")
	tb.Cleanup(func() { _ = engine.Close() })
	return engine
}

// assertAggregateCounts verifies that every key in want exists in got with the
// expected count, and that there are no extra rows.
func assertAggregateCounts(tb testing.TB, got []AggregateRow, want map[string]int64) {
	tb.Helper()
	gotMap := make(map[string]int64, len(got))
	for _, r := range got {
		_, seen := gotMap[r.Key]
		assert.False(tb, seen, "duplicate key %q in aggregate results", r.Key)
		gotMap[r.Key] = r.Count
	}
	for key, wantCount := range want {
		gotCount, ok := gotMap[key]
		if !assert.True(tb, ok, "missing expected key %q", key) {
			continue
		}
		assert.Equal(tb, wantCount, gotCount, "key %q count", key)
	}
	for _, r := range got {
		_, ok := want[r.Key]
		assert.True(tb, ok, "unexpected key %q (count=%d)", r.Key, r.Count)
	}
}

// assertDescendingOrder verifies that aggregate results are sorted by count descending.
func assertDescendingOrder(tb testing.TB, got []AggregateRow) {
	tb.Helper()
	for i := 1; i < len(got); i++ {
		assert.LessOrEqual(tb, got[i].Count, got[i-1].Count,
			"results not in descending order: %q (count=%d) after %q (count=%d)",
			got[i].Key, got[i].Count, got[i-1].Key, got[i-1].Count)
	}
}

// makeDate creates a time.Time for the given month, day in 2024, UTC, zero time.
func makeDate(month, day int) time.Time {
	return time.Date(2024, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}
