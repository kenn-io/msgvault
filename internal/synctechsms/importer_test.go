package synctechsms

import (
	"path/filepath"
	"testing"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImporterImportsSMSMMSCallsAndIsIdempotent(t *testing.T) {
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
	if err != nil {
		t.Fatalf("ImportPath: %v", err)
	}
	if summary.SMSImported != 1 || summary.MMSImported != 1 || summary.CallsImported != 1 || summary.AttachmentsImported != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	writeFile(t, filepath.Join(dir, "messages-copy.xml"), `<smses count="1">
  <sms address="+15551234567" date="1717214400000" type="1" body="hello from sms" read="1" status="-1" contact_name="Alice" />
</smses>`)
	summary, err = imp.ImportPath(dir)
	if err != nil {
		t.Fatalf("ImportPath second: %v", err)
	}
	assertMessageCount(t, f.Store, 3)
	assertRawFormats(t, f.Store, RawFormat, 3)
}

func TestImporterRejectsMissingOwnerPhone(t *testing.T) {
	f := storetest.New(t)
	imp := NewImporter(f.Store, ImportOptions{IncludeSMS: true})
	_, err := imp.ImportPath(t.TempDir())
	if err == nil {
		t.Fatal("ImportPath returned nil error")
	}
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
	if err != nil {
		t.Fatalf("ImportPath: %v", err)
	}
	if summary.CallsImported != 1 {
		t.Fatalf("CallsImported = %d, want 1", summary.CallsImported)
	}
	assertMessageCount(t, f.Store, 1)
}

func assertMessageCount(t *testing.T, st *store.Store, want int) {
	t.Helper()
	var got int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&got); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
}

func assertRawFormats(t *testing.T, st *store.Store, format string, want int) {
	t.Helper()
	var got int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM message_raw WHERE raw_format = ?`, format).Scan(&got); err != nil {
		t.Fatalf("count raw formats: %v", err)
	}
	if got != want {
		t.Fatalf("raw format count = %d, want %d", got, want)
	}
}
