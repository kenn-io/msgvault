package synctechsms

import (
	"strings"
	"testing"
	"time"
)

func TestParseSMSBackup(t *testing.T) {
	xml := `<smses count="2" backup_set="abc" backup_date="1717214400000">
  <sms protocol="0" address="+15551234567" date="1717214400123" type="1" subject="null" body="hello" toa="null" sc_toa="null" service_center="null" read="1" status="-1" readable_date="Jun 1, 2024 4:00:00 AM" contact_name="Alice" />
  <sms protocol="0" address="12345" date="1717214460000" type="2" subject="null" body="short code reply" toa="null" sc_toa="null" service_center="null" read="0" status="-1" readable_date="Jun 1, 2024 4:01:00 AM" contact_name="null" />
</smses>`
	doc, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Kind != KindMessages {
		t.Fatalf("Kind = %q, want %q", doc.Kind, KindMessages)
	}
	if len(doc.SMS) != 2 {
		t.Fatalf("len(SMS) = %d, want 2", len(doc.SMS))
	}
	if doc.SMS[0].Address != "+15551234567" || doc.SMS[0].Body != "hello" || doc.SMS[0].Type != SMSTypeInbox {
		t.Fatalf("first SMS parsed incorrectly: %#v", doc.SMS[0])
	}
	want := time.UnixMilli(1717214400123).UTC()
	if !doc.SMS[0].Timestamp.Equal(want) {
		t.Fatalf("Timestamp = %s, want %s", doc.SMS[0].Timestamp, want)
	}
	if doc.SMS[0].Subject.Valid {
		t.Fatalf("literal null subject should become invalid NullString")
	}
	if doc.SMS[1].Address != "12345" {
		t.Fatalf("short code address = %q, want 12345", doc.SMS[1].Address)
	}
}

func TestParseMMSBackupWithTextMediaAndRecipients(t *testing.T) {
	xml := `<smses count="1">
  <mms date="1717214520000" msg_box="1" read="1" m_id="mms-1" sub="Group subject" ct_t="application/vnd.wap.multipart.related" m_type="132" readable_date="Jun 1, 2024 4:02:00 AM" contact_name="Group">
    <parts>
      <part seq="0" ct="text/plain" text="photo caption" />
      <part seq="1" ct="image/png" cl="image.png" data="aGVsbG8=" />
    </parts>
    <addrs>
      <addr address="+15550000001" type="137" charset="106" />
      <addr address="+15550000002" type="151" charset="106" />
      <addr address="+15550000003" type="151" charset="106" />
    </addrs>
  </mms>
</smses>`
	doc, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(doc.MMS) != 1 {
		t.Fatalf("len(MMS) = %d, want 1", len(doc.MMS))
	}
	m := doc.MMS[0]
	if m.MessageBox != MMSBoxInbox || m.Subject.String != "Group subject" || !m.Subject.Valid {
		t.Fatalf("MMS metadata parsed incorrectly: %#v", m)
	}
	if len(m.Parts) != 2 || m.Parts[1].ContentType != "image/png" || string(m.Parts[1].Data) != "hello" {
		t.Fatalf("MMS parts parsed incorrectly: %#v", m.Parts)
	}
	if len(m.Addresses) != 3 || m.Addresses[0].Type != MMSAddressFrom || m.Addresses[1].Type != MMSAddressTo {
		t.Fatalf("MMS addresses parsed incorrectly: %#v", m.Addresses)
	}
}

func TestParseCallLog(t *testing.T) {
	xml := `<calls count="1" backup_set="abc" backup_date="1717218000000">
  <call number="+15551234567" duration="42" date="1717218000123" type="3" presentation="1" readable_date="Jun 1, 2024 5:00:00 AM" contact_name="Alice" />
</calls>`
	doc, err := Parse(strings.NewReader(xml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Kind != KindCalls {
		t.Fatalf("Kind = %q, want %q", doc.Kind, KindCalls)
	}
	if len(doc.Calls) != 1 {
		t.Fatalf("len(Calls) = %d, want 1", len(doc.Calls))
	}
	if doc.Calls[0].Type != CallMissed || doc.Calls[0].DurationSeconds != 42 {
		t.Fatalf("call parsed incorrectly: %#v", doc.Calls[0])
	}
}

func TestParseRejectsUnsupportedRoot(t *testing.T) {
	_, err := Parse(strings.NewReader(`<backup></backup>`))
	if err == nil {
		t.Fatal("Parse returned nil error for unsupported root")
	}
	if !strings.Contains(err.Error(), "unsupported SMS Backup & Restore XML root") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestParseRejectsInvalidBase64Part(t *testing.T) {
	xml := `<smses count="1"><mms date="1" msg_box="1"><parts><part seq="0" ct="image/png" data="%%%"/></parts></mms></smses>`
	_, err := Parse(strings.NewReader(xml))
	if err == nil {
		t.Fatal("Parse returned nil error for invalid base64")
	}
	if !strings.Contains(err.Error(), "decode MMS part data") {
		t.Fatalf("error = %q", err.Error())
	}
}
