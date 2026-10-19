package synctechsms

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImporterImportsSMSMMSCallsAndIsIdempotent(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="2">
  <sms address="+15551234567" date="1717214400000" type="1" body="hello from sms" read="1" status="-1" contact_name="Alice" />
  <mms date="1717214460000" msg_box="2" read="1" m_id="mms-1" sub="null">
    <parts>
      <part seq="0" ct="text/plain" text="mms text" />
      <part seq="1" ct="image/png" cl="image.png" data="aGVsbG8=" />
    </parts>
    <addrs>
      <addr address="+15550000001" type="137" charset="106" />
      <addr address="+15551234567" type="151" charset="106" />
    </addrs>
  </mms>
</smses>`)
	writeFile(t, filepath.Join(dir, "calls.xml"), `<calls count="1">
  <call number="+15551234567" duration="42" date="1717218000000" type="3" presentation="1" contact_name="Alice" />
</calls>`)

	imp := NewImporter(f.Store, ImportOptions{
		OwnerPhone:         "+15550000001",
		AttachmentsDir:     filepath.Join(dir, "attachments"),
		IncludeSMS:         true,
		IncludeMMS:         true,
		IncludeCalls:       true,
		IncludeAttachments: true,
	})
	summary, err := imp.ImportPath(dir)
	require.NoError(err, "ImportPath")
	require.Equal(1, summary.SMSImported, "summary = %#v", summary)
	require.Equal(1, summary.MMSImported, "summary = %#v", summary)
	require.Equal(1, summary.CallsImported, "summary = %#v", summary)
	require.Equal(1, summary.AttachmentsImported, "summary = %#v", summary)
	// SMS and MMS with the same other party share one conversation; calls
	// stay on a separate thread.
	assertConversationCount(t, f.Store, 2)
	// The MMS parent message must record attachment metadata so the UI
	// shows an attachment badge and attachment-only filters match.
	assertMMSHasAttachmentMetadata(t, f.Store, 1)
	// MMS attachments must land at the canonical content-addressed path
	// (<hash[:2]>/<hash>), not the legacy synctech-sms/ namespace, so they
	// are readable through the standard read paths.
	assertAttachmentAtCanonicalPath(t, f.Store, filepath.Join(dir, "attachments"))
	writeFile(t, filepath.Join(dir, "messages-copy.xml"), `<smses count="1">
  <sms address="+15551234567" date="1717214400000" type="1" body="hello from sms" read="1" status="-1" contact_name="Alice" />
</smses>`)
	_, err = imp.ImportPath(dir)
	require.NoError(err, "ImportPath second")
	assertMessageCount(t, f.Store, 3)
	assertRawFormats(t, f.Store, RawFormat, 3)
}

func TestImporterRejectsMissingOwnerPhone(t *testing.T) {
	f := storetest.New(t)
	imp := NewImporter(f.Store, ImportOptions{IncludeSMS: true})
	_, err := imp.ImportPath(t.TempDir())
	require.Error(t, err, "ImportPath returned nil error")
}

func TestImporterContinuesWhenFTSUpsertFails(t *testing.T) {
	testutil.SkipIfPostgres(t, "drops SQLite's messages_fts virtual table; PostgreSQL FTS is a messages.search_fts column")
	f := storetest.New(t)
	if f.Store.FTS5Available() {
		_, err := f.Store.DB().Exec(`DROP TABLE messages_fts`)
		require.NoError(t, err, "drop FTS table")
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="1">
  <sms address="+15551234567" date="1717214400000" type="1" body="hello from sms" read="1" status="-1" contact_name="Alice" />
</smses>`)

	imp := NewImporter(f.Store, ImportOptions{
		OwnerPhone: "+15550000001",
		IncludeSMS: true,
	})
	summary, err := imp.ImportPath(dir)
	require.NoError(t, err, "ImportPath")
	assert.Equal(t, 1, summary.SMSImported, "summary = %#v", summary)
	assertMessageCount(t, f.Store, 1)
}

func TestImporterImportsCallWithBlankNumber(t *testing.T) {
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "calls.xml"), `<calls count="1">
  <call number="" duration="0" date="1775245887101" type="5" presentation="3" contact_name="(Unknown)" />
</calls>`)

	imp := NewImporter(f.Store, ImportOptions{
		OwnerPhone:   "+15550000001",
		IncludeCalls: true,
	})
	summary, err := imp.ImportPath(dir)
	require.NoError(t, err, "ImportPath")
	require.Equal(t, 1, summary.CallsImported)
	assertMessageCount(t, f.Store, 1)
}

func TestImporterReturnsMMSMessageIDWhenAttachmentImportFails(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="1">
  <mms date="1717214460000" msg_box="2" read="1" m_id="mms-attachment-fails" sub="null">
    <parts>
      <part seq="0" ct="text/plain" text="mms text" />
      <part seq="1" ct="image/png" cl="image.png" data="aGVsbG8=" />
    </parts>
    <addrs>
      <addr address="+15550000001" type="137" charset="106" />
      <addr address="+15551234567" type="151" charset="106" />
    </addrs>
  </mms>
</smses>`)

	imp := NewImporter(f.Store, ImportOptions{
		OwnerPhone:         "+15550000001",
		AttachmentsDir:     filepath.Join(dir, "attachments"),
		MaxAttachmentBytes: 1,
		IncludeMMS:         true,
		IncludeAttachments: true,
	})
	summary, err := imp.ImportPath(dir)
	require.Error(err, "ImportPath")
	assert.Contains(err.Error(), "exceeds maximum size", "ImportPath error")

	assertMessageCount(t, f.Store, 1)
	require.Len(summary.MessageIDs, 1, "summary message IDs")
	assert.Positive(summary.MessageIDs[0], "summary message id")
}

// TestImporterMarksDraftsAsFromMe guards against regressing the
// draft-handling fix. SMSTypeDraft and MMSBoxDraft are owner-authored
// messages; treating them as incoming hides them on the wrong side of
// the conversation in TUI/API renderings of is_from_me.
func TestImporterMarksDraftsAsFromMe(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="2">
  <sms address="+15551234567" date="1717214400000" type="3" body="draft sms reply" read="1" status="-1" contact_name="Alice" />
  <mms date="1717214460000" msg_box="3" read="1" m_id="mms-draft" sub="null">
    <parts>
      <part seq="0" ct="text/plain" text="draft mms reply" />
    </parts>
    <addrs>
      <addr address="+15550000001" type="137" charset="106" />
      <addr address="+15551234567" type="151" charset="106" />
    </addrs>
  </mms>
</smses>`)

	imp := NewImporter(f.Store, ImportOptions{
		OwnerPhone: "+15550000001",
		IncludeSMS: true,
		IncludeMMS: true,
	})
	_, err := imp.ImportPath(dir)
	require.NoError(err, "ImportPath")

	rows, err := f.Store.DB().Query(`SELECT source_message_id, is_from_me FROM messages WHERE message_type IN ('sms', 'mms')`)
	require.NoError(err, "query messages")
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var srcID string
		var fromMe bool
		require.NoError(rows.Scan(&srcID, &fromMe), "scan")
		got[srcID] = fromMe
	}
	require.NoError(rows.Err(), "messages rows")
	require.Len(got, 2, "got %#v", got)
	for srcID, fromMe := range got {
		assert.True(t, fromMe, "%s is_from_me = false, want true (draft)", srcID)
	}
}

func TestImporterRemovesUnsentMMSLabelWhenReimportedAsSent(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="1">
  <mms date="1717214460000" msg_box="4" read="1" m_id="mms-transition">
    <parts><part seq="0" ct="text/plain" text="outgoing update" /></parts>
    <addrs><addr address="+15550000001" type="137" /><addr address="+15551234567" type="151" /></addrs>
  </mms>
</smses>`)

	options := ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true}
	_, err := NewImporter(f.Store, options).ImportPath(dir)
	requirements.NoError(err, "import outbox MMS")

	var sourceID int64
	requirements.NoError(f.Store.DB().QueryRow(`SELECT id FROM sources WHERE source_type = ?`, SourceType).Scan(&sourceID))
	var firstMessageID int64
	var isSent, hasUnsentLabel bool
	const stateQuery = `SELECT m.id, COALESCE(m.is_sent, FALSE), EXISTS(
		SELECT 1 FROM message_labels ml JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = m.id AND l.source_id = m.source_id AND l.source_label_id = 'synctech-unsent'
	) FROM messages m WHERE m.source_id = ? AND m.message_type = 'mms'`
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(stateQuery), sourceID).Scan(&firstMessageID, &isSent, &hasUnsentLabel))
	assertions.False(isSent)
	assertions.True(hasUnsentLabel)

	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="1">
  <mms date="1717214460000" msg_box="2" read="1" m_id="mms-transition">
    <parts><part seq="0" ct="text/plain" text="outgoing update" /></parts>
    <addrs><addr address="+15550000001" type="137" /><addr address="+15551234567" type="151" /></addrs>
  </mms>
</smses>`)
	_, err = NewImporter(f.Store, options).ImportPath(dir)
	requirements.NoError(err, "reimport sent MMS")

	var secondMessageID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(stateQuery), sourceID).Scan(&secondMessageID, &isSent, &hasUnsentLabel))
	assertions.Equal(firstMessageID, secondMessageID, "stable MMS identity should update the existing message")
	assertions.True(isSent, "reimported MMS should be marked sent")
	assertions.False(hasUnsentLabel, "sent MMS should not retain synthetic unsent evidence")
}

func TestImporterPreservesUnsentSMSMMSAndExcludesCallLogsFromLoops(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="9">
  <sms address="+15551234567" date="1717214400000" type="3" body="sms draft" contact_name="Alice" />
  <sms address="+15551234567" date="1717214460000" type="4" body="sms outbox" contact_name="Alice" />
  <sms address="+15551234567" date="1717214520000" type="5" body="sms failed" contact_name="Alice" />
  <sms address="+15551234567" date="1717214580000" type="6" body="sms queued" contact_name="Alice" />
  <sms address="+15551234567" date="1717214640000" type="2" body="sms sent" contact_name="Alice" />
  <sms address="+15551234567" date="1717214700000" type="1" body="sms inbound" contact_name="Alice" />
  <mms date="1717214760000" msg_box="3" m_id="mms-draft"><parts><part seq="0" ct="text/plain" text="mms draft" /></parts><addrs><addr address="+15550000001" type="137" /><addr address="+15551234567" type="151" /></addrs></mms>
  <mms date="1717214820000" msg_box="4" m_id="mms-outbox"><parts><part seq="0" ct="text/plain" text="mms outbox" /></parts><addrs><addr address="+15550000001" type="137" /><addr address="+15551234567" type="151" /></addrs></mms>
  <mms date="1717214880000" msg_box="2" m_id="mms-sent"><parts><part seq="0" ct="text/plain" text="mms sent" /></parts><addrs><addr address="+15550000001" type="137" /><addr address="+15551234567" type="151" /></addrs></mms>
</smses>`)
	writeFile(t, filepath.Join(dir, "calls.xml"), `<calls count="1"><call number="+15551234567" duration="42" date="1717214940000" type="1" contact_name="Alice" /></calls>`)
	imp := NewImporter(f.Store, ImportOptions{
		OwnerPhone: "+15550000001", IncludeSMS: true, IncludeMMS: true, IncludeCalls: true,
	})
	_, err := imp.ImportPath(dir)
	requirements.NoError(err)

	var sourceID int64
	requirements.NoError(f.Store.DB().QueryRow("SELECT id FROM sources WHERE source_type='synctech_sms'").Scan(&sourceID))
	rows, err := f.Store.DB().Query(f.Store.Rebind(`SELECT m.snippet, COALESCE(m.source_is_from_me,FALSE), EXISTS(
		SELECT 1 FROM message_labels ml JOIN labels l ON l.id=ml.label_id
		WHERE ml.message_id=m.id AND l.source_id=m.source_id AND l.system_role='drafts'
	) FROM messages m WHERE m.source_id=? AND m.message_type IN ('sms','mms')`), sourceID)
	requirements.NoError(err)
	persisted := make(map[string]struct {
		fromMe bool
		unsent bool
	})
	for rows.Next() {
		var body string
		var fromMe, unsent bool
		requirements.NoError(rows.Scan(&body, &fromMe, &unsent))
		persisted[body] = struct {
			fromMe bool
			unsent bool
		}{fromMe: fromMe, unsent: unsent}
	}
	requirements.NoError(rows.Err())
	requirements.NoError(rows.Close())
	requirements.Len(persisted, 9)
	for _, body := range []string{"sms draft", "sms outbox", "sms failed", "sms queued", "mms draft", "mms outbox"} {
		assertions.True(persisted[body].fromMe, "%s should retain owner authorship", body)
		assertions.True(persisted[body].unsent, "%s must retain native unsent evidence", body)
	}
	for _, body := range []string{"sms sent", "mms sent"} {
		assertions.True(persisted[body].fromMe, "%s should retain owner authorship", body)
		assertions.False(persisted[body].unsent, "%s is delivered evidence", body)
	}
	assertions.False(persisted["sms inbound"].fromMe)
	assertions.False(persisted["sms inbound"].unsent)

	var textConversationID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT id FROM conversations WHERE source_id=? AND source_conversation_id LIKE 'text:%'"), sourceID).Scan(&textConversationID))
	view, err := f.Store.LoadLoopConversationContext(t.Context(), textConversationID, 20)
	requirements.NoError(err)
	loaded := make(map[string]bool)
	for _, message := range view.Messages {
		loaded[message.Snippet] = true
	}
	for _, body := range []string{"sms draft", "sms outbox", "sms failed", "sms queued", "mms draft", "mms outbox"} {
		assertions.NotContains(loaded, body, "%s must not enter turn derivation", body)
	}
	for _, body := range []string{"sms sent", "mms sent", "sms inbound"} {
		assertions.Contains(loaded, body)
	}
	requirements.Len(loaded, 3)

	var callConversationID int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT id FROM conversations WHERE source_id=? AND source_conversation_id LIKE 'calls:%'"), sourceID).Scan(&callConversationID))
	callView, err := f.Store.LoadLoopConversationContext(t.Context(), callConversationID, 20)
	requirements.NoError(err)
	assertions.Empty(callView.Messages, "call records do not represent unanswered conversation turns")
}

func assertMessageCount(t *testing.T, st *store.Store, want int) {
	t.Helper()
	var got int
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&got), "count messages")
	require.Equal(t, want, got, "message count")
}

func assertRawFormats(t *testing.T, st *store.Store, format string, want int) {
	t.Helper()
	var got int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_raw WHERE raw_format = ?`), format).Scan(&got), "count raw formats")
	require.Equal(t, want, got, "raw format count")
}

func assertConversationCount(t *testing.T, st *store.Store, want int) {
	t.Helper()
	var got int
	// Filter to conversations created by this importer; the shared store
	// fixture seeds a default-thread row that is unrelated.
	require.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM conversations WHERE source_conversation_id LIKE 'text:%' OR source_conversation_id LIKE 'calls:%'`).Scan(&got), "count conversations")
	require.Equal(t, want, got, "conversation count")
}

// assertAttachmentAtCanonicalPath asserts that the sole attachment row's
// storage_path is the canonical <hash[:2]>/<hash> form (not the legacy
// synctech-sms/ namespace) and that the file exists on disk at that path.
func assertAttachmentAtCanonicalPath(t *testing.T, st *store.Store, attachmentsDir string) {
	t.Helper()
	var storagePath, contentHash, role, roleSource, sourcePartKey string
	err := st.DB().QueryRow(`
		SELECT storage_path, content_hash, attachment_role, role_source, source_part_key
		FROM attachments ORDER BY id LIMIT 1`).
		Scan(&storagePath, &contentHash, &role, &roleSource, &sourcePartKey)
	require.NoError(t, err, "read attachment row")
	require.NotEmpty(t, contentHash, "content_hash")
	// storage_path is stored slash-separated on every platform.
	wantPath := contentHash[:2] + "/" + contentHash
	assert.Equal(t, wantPath, storagePath, "storage_path")
	assert.NotContains(t, storagePath, "synctech-sms", "storage_path must not use legacy namespace")
	assert.Equal(t, "standalone", role)
	assert.Equal(t, "importer_semantics", roleSource)
	assert.NotEmpty(t, sourcePartKey)
	_, err = os.Stat(filepath.Join(attachmentsDir, contentHash[:2], contentHash))
	assert.NoError(t, err, "attachment file at canonical path")
}

func assertMMSHasAttachmentMetadata(t *testing.T, st *store.Store, wantCount int) {
	t.Helper()
	var hasAttachments bool
	var count int
	err := st.DB().QueryRow(`SELECT has_attachments, attachment_count FROM messages WHERE message_type = 'mms'`).Scan(&hasAttachments, &count)
	require.NoError(t, err, "read mms attachment metadata")
	require.True(t, hasAttachments, "mms metadata: has_attachments=%v count=%d, want true/%d", hasAttachments, count, wantCount)
	require.Equal(t, wantCount, count, "mms metadata: has_attachments=%v count=%d, want true/%d", hasAttachments, count, wantCount)
}
