package synctechsms

import (
	"strings"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
)

func TestParseSMSBackup(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	xml := `<smses count="2" backup_set="abc" backup_date="1717214400000">
  <sms protocol="0" address="+15551234567" date="1717214400123" type="1" subject="null" body="hello" toa="null" sc_toa="null" service_center="null" read="1" status="-1" readable_date="Jun 1, 2024 4:00:00 AM" contact_name="Alice" />
  <sms protocol="0" address="12345" date="1717214460000" type="2" subject="null" body="short code reply" toa="null" sc_toa="null" service_center="null" read="0" status="-1" readable_date="Jun 1, 2024 4:01:00 AM" contact_name="null" />
</smses>`
	doc, err := Parse(strings.NewReader(xml))
	require.NoError(err, "Parse")
	require.Equal(KindMessages, doc.Kind)
	require.Len(doc.SMS, 2)
	assert.Equal("+15551234567", doc.SMS[0].Address, "first SMS parsed incorrectly: %#v", doc.SMS[0])
	assert.Equal("hello", doc.SMS[0].Body, "first SMS parsed incorrectly: %#v", doc.SMS[0])
	assert.Equal(SMSTypeInbox, doc.SMS[0].Type, "first SMS parsed incorrectly: %#v", doc.SMS[0])
	want := time.UnixMilli(1717214400123).UTC()
	assert.True(doc.SMS[0].Timestamp.Equal(want), "Timestamp = %s, want %s", doc.SMS[0].Timestamp, want)
	assert.False(doc.SMS[0].Subject.Valid, "literal null subject should become invalid NullString")
	assert.Equal("12345", doc.SMS[1].Address, "short code address")
}

func TestParseMMSBackupWithTextMediaAndRecipients(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
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
	require.NoError(err, "Parse")
	require.Len(doc.MMS, 1)
	m := doc.MMS[0]
	assert.Equal(MMSBoxInbox, m.MessageBox, "MMS metadata parsed incorrectly: %#v", m)
	assert.Equal("Group subject", m.Subject.String, "MMS metadata parsed incorrectly: %#v", m)
	assert.True(m.Subject.Valid, "MMS metadata parsed incorrectly: %#v", m)
	require.Len(m.Parts, 2, "MMS parts: %#v", m.Parts)
	assert.Equal("image/png", m.Parts[1].ContentType, "MMS parts: %#v", m.Parts)
	assert.Equal("hello", string(m.Parts[1].Data), "MMS parts: %#v", m.Parts)
	require.Len(m.Addresses, 3, "MMS addresses: %#v", m.Addresses)
	assert.Equal(MMSAddressFrom, m.Addresses[0].Type, "MMS addresses: %#v", m.Addresses)
	assert.Equal(MMSAddressTo, m.Addresses[1].Type, "MMS addresses: %#v", m.Addresses)
}

func TestParseCallLog(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	xml := `<calls count="1" backup_set="abc" backup_date="1717218000000">
  <call number="+15551234567" duration="42" date="1717218000123" type="3" presentation="1" readable_date="Jun 1, 2024 5:00:00 AM" contact_name="Alice" />
</calls>`
	doc, err := Parse(strings.NewReader(xml))
	require.NoError(err, "Parse")
	require.Equal(KindCalls, doc.Kind)
	require.Len(doc.Calls, 1)
	assert.Equal(CallMissed, doc.Calls[0].Type, "call: %#v", doc.Calls[0])
	assert.Equal(42, doc.Calls[0].DurationSeconds, "call: %#v", doc.Calls[0])
}

func TestParseRejectsUnsupportedRoot(t *testing.T) {
	_, err := Parse(strings.NewReader(`<backup></backup>`))
	requirepkg.Error(t, err, "Parse returned nil error for unsupported root")
	assertpkg.ErrorContains(t, err, "unsupported SMS Backup & Restore XML root")
}

func TestParseRejectsInvalidBase64Part(t *testing.T) {
	xml := `<smses count="1"><mms date="1" msg_box="1"><parts><part seq="0" ct="image/png" data="%%%"/></parts></mms></smses>`
	_, err := Parse(strings.NewReader(xml))
	requirepkg.Error(t, err, "Parse returned nil error for invalid base64")
	assertpkg.ErrorContains(t, err, "decode MMS part data")
}
