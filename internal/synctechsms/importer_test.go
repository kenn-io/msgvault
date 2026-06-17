package synctechsms

import (
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImporterImportsSMSMMSCallsAndIsIdempotent(t *testing.T) {
	require := requirepkg.New(t)
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
	requirepkg.Error(t, err, "ImportPath returned nil error")
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
	requirepkg.NoError(t, err, "ImportPath")
	requirepkg.Equal(t, 1, summary.CallsImported)
	assertMessageCount(t, f.Store, 1)
}

// TestImporterMarksDraftsAsFromMe guards against regressing the
// draft-handling fix. SMSTypeDraft and MMSBoxDraft are owner-authored
// messages; treating them as incoming hides them on the wrong side of
// the conversation in TUI/API renderings of is_from_me.
func TestImporterMarksDraftsAsFromMe(t *testing.T) {
	require := requirepkg.New(t)
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
		assertpkg.True(t, fromMe, "%s is_from_me = false, want true (draft)", srcID)
	}
}

func assertMessageCount(t *testing.T, st *store.Store, want int) {
	t.Helper()
	var got int
	requirepkg.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&got), "count messages")
	requirepkg.Equal(t, want, got, "message count")
}

func assertRawFormats(t *testing.T, st *store.Store, format string, want int) {
	t.Helper()
	var got int
	requirepkg.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM message_raw WHERE raw_format = ?`), format).Scan(&got), "count raw formats")
	requirepkg.Equal(t, want, got, "raw format count")
}

func assertConversationCount(t *testing.T, st *store.Store, want int) {
	t.Helper()
	var got int
	// Filter to conversations created by this importer; the shared store
	// fixture seeds a default-thread row that is unrelated.
	requirepkg.NoError(t, st.DB().QueryRow(`SELECT COUNT(*) FROM conversations WHERE source_conversation_id LIKE 'text:%' OR source_conversation_id LIKE 'calls:%'`).Scan(&got), "count conversations")
	requirepkg.Equal(t, want, got, "conversation count")
}

func assertMMSHasAttachmentMetadata(t *testing.T, st *store.Store, wantCount int) {
	t.Helper()
	var hasAttachments bool
	var count int
	err := st.DB().QueryRow(`SELECT has_attachments, attachment_count FROM messages WHERE message_type = 'mms'`).Scan(&hasAttachments, &count)
	requirepkg.NoError(t, err, "read mms attachment metadata")
	requirepkg.True(t, hasAttachments, "mms metadata: has_attachments=%v count=%d, want true/%d", hasAttachments, count, wantCount)
	requirepkg.Equal(t, wantCount, count, "mms metadata: has_attachments=%v count=%d, want true/%d", hasAttachments, count, wantCount)
}
