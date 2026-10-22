package synctechsms

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechstate"
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
	assertRawFormats(t, f.Store, synctechstate.RawFormat, 3)
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

func TestImporterKeepsSynctechUnsentSMSOutOfLoopDetection(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="7">
  <sms address="+15551234567" date="1717214400000" type="1" body="Can you send the contract?" read="1" status="-1" contact_name="Alice" />
  <sms address="+15551234567" date="1717214460000" type="3" body="Draft reply" read="1" status="-1" contact_name="Alice" />
  <sms address="+15551234567" date="1717214520000" type="4" body="Outbox reply" read="1" status="-1" contact_name="Alice" />
  <sms address="+15551234567" date="1717214580000" type="5" body="Failed reply" read="1" status="-1" contact_name="Alice" />
  <sms address="+15551234567" date="1717214640000" type="6" body="Queued reply" read="1" status="-1" contact_name="Alice" />
  <mms date="1717214700000" msg_box="3" read="1" m_id="draft-mms" sub="null">
    <parts><part seq="0" ct="text/plain" text="Draft MMS reply" /></parts>
    <addrs>
      <addr address="+15550000001" type="137" charset="106" />
      <addr address="+15551234567" type="151" charset="106" />
    </addrs>
  </mms>
  <mms date="1717214760000" msg_box="4" read="1" m_id="outbox-mms" sub="null">
    <parts><part seq="0" ct="text/plain" text="Outbox MMS reply" /></parts>
    <addrs>
      <addr address="+15550000001" type="137" charset="106" />
      <addr address="+15551234567" type="151" charset="106" />
    </addrs>
  </mms>
</smses>`)
	imp := NewImporter(f.Store, ImportOptions{OwnerPhone: "+15550000001", IncludeSMS: true, IncludeMMS: true})
	_, err := imp.ImportPath(dir)
	require.NoError(err)

	var questionID int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT id FROM messages WHERE message_type = 'sms' AND snippet = ?`),
		"Can you send the contract?",
	).Scan(&questionID))
	rows, err := f.Store.DB().QueryContext(t.Context(), f.Store.Rebind(
		`SELECT is_from_me, is_sent FROM messages WHERE message_type IN ('sms', 'mms') AND snippet <> ?`),
		"Can you send the contract?",
	)
	require.NoError(err)
	defer func() { _ = rows.Close() }()
	var unsent int
	for rows.Next() {
		var isFromMe, isSent bool
		require.NoError(rows.Scan(&isFromMe, &isSent))
		assert.True(isFromMe, "provider-native unsent SMS stays on the owner's side")
		assert.False(isSent, "provider-native unsent SMS is excluded from live loops")
		unsent++
	}
	require.NoError(rows.Err())
	require.Equal(6, unsent)

	result, err := loops.List(t.Context(), f.Store, loops.Options{}, time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.Len(result.Loops, 1, "unsent drafts and delivery states must not settle the incoming question")
	assert.Equal(questionID, result.Loops[0].Anchor.ID)
	assert.Equal(loops.ReplyOwed, result.Loops[0].Kind)
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

func TestImportedGroupMMSDoesNotOpenLoops(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="3">
  <mms date="1717214460000" msg_box="2" m_id="group-sent">
    <parts><part seq="0" ct="text/plain" text="Can you review the draft?" /></parts>
    <addrs><addr address="+15550000001" type="137" /><addr address="+15550000002" type="151" /><addr address="+15550000003" type="151" /></addrs>
  </mms>
  <mms date="1717214460000" msg_box="1" m_id="group-received">
    <parts><part seq="0" ct="text/plain" text="Can you send the budget?" /></parts>
    <addrs><addr address="+15550000004" type="137" /><addr address="+15550000001" type="151" /><addr address="+15550000005" type="151" /></addrs>
  </mms>
  <mms date="1717214460000" msg_box="1" m_id="direct-received">
    <parts><part seq="0" ct="text/plain" text="Can you send the contract?" /></parts>
    <addrs><addr address="+15550000006" type="137" /><addr address="+15550000001" type="151" /></addrs>
  </mms>
</smses>`)
	imp := NewImporter(f.Store, ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true})
	_, err := imp.ImportPath(dir)
	require.NoError(err)
	result, err := loops.List(t.Context(), f.Store, loops.Options{}, time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.Len(result.Loops, 1, "only the one-to-one question opens a loop")
	assert.Equal("+15550000006", result.Loops[0].Counterparty.Address)
	// The incoming control also lists when the owner has no linked identity.
	source, err := f.Store.GetOrCreateSource(SourceType, "+15550000001")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(source.ID, "+15550000001", "manual"))
	result, err = loops.List(t.Context(), f.Store, loops.Options{}, time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.Len(result.Loops, 1, "recognizing the owner still excludes both groups")
	first, err := f.Store.EnsureParticipantByPhone("+15550000002", "", "phone")
	require.NoError(err)
	second, err := f.Store.EnsureParticipantByPhone("+15550000003", "", "phone")
	require.NoError(err)
	_, err = f.Store.LinkParticipants(first, second)
	require.NoError(err)
	_, _, err = f.Store.CreatePersonFromParticipant(first)
	require.NoError(err)
	result, err = loops.List(t.Context(), f.Store, loops.Options{}, time.Date(2024, 6, 10, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	assert.Len(result.Loops, 2, "two addresses linked to one person form a direct chat")
}
