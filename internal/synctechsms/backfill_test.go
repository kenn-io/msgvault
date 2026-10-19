package synctechsms

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestBackfillNativeSendStateResumesInBoundedBatches(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	const unsentSMSCount = store.SynctechSMSNativeStateBackfillBatchSize + 1
	const unsentMMSCount = 1
	const totalUnsentCount = unsentSMSCount + unsentMMSCount
	var xml strings.Builder
	fmt.Fprintf(&xml, `<smses count="%d">`, unsentSMSCount+2)
	for i := range unsentSMSCount {
		fmt.Fprintf(&xml, `<sms address="+15551234567" date="%d" type="4" body="queued %03d" read="1" status="-1" />`, 1717214400000+int64(i)*1000, i)
	}
	fmt.Fprintf(&xml, `<sms address="+15551234567" date="%d" type="2" body="sent control" read="1" status="-1" />`, 1717214400000+int64(unsentSMSCount)*1000)
	fmt.Fprintf(&xml, `<mms date="%d" msg_box="3" read="1" m_id="mms-draft" sub="null"><parts><part seq="0" ct="image/png" cl="image.png" data="aGVsbG8=" /></parts><addrs><addr address="+15550000001" type="137" charset="106" /><addr address="+15551234567" type="151" charset="106" /></addrs></mms>`, 1717214400000+int64(unsentSMSCount+1)*1000)
	xml.WriteString(`</smses>`)
	writeFile(t, filepath.Join(dir, "messages.xml"), xml.String())
	_, err := NewImporter(f.Store, ImportOptions{OwnerPhone: "+15550000001", IncludeSMS: true, IncludeMMS: true}).ImportPath(dir)
	requirements.NoError(err)
	source, err := f.Store.GetOrCreateSource(SourceType, "+15550000001")
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET is_sent=TRUE WHERE source_id=? AND message_type IN ('sms','mms')`), source.ID)
	requirements.NoError(err)
	var expectedCursor int64
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT m.id FROM messages m JOIN sources src ON src.id=m.source_id
		JOIN message_raw r ON r.message_id=m.id
		WHERE src.source_type='synctech_sms' AND m.message_type IN ('sms','mms')
		AND m.source_is_from_me=TRUE AND COALESCE(m.is_sent,TRUE)=TRUE
		AND r.raw_format='synctech_sms_xml_json'
		ORDER BY m.id LIMIT 1 OFFSET ?`), store.SynctechSMSNativeStateBackfillBatchSize-1).Scan(&expectedCursor))

	first, err := BackfillNativeSendStateContext(t.Context(), f.Store)
	requirements.NoError(err)
	assertions.Positive(first)
	assertions.Less(first, totalUnsentCount, "a deriver pass must process only one bounded batch")
	var corrected int
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND message_type IN ('sms','mms') AND is_sent=FALSE`), source.ID).Scan(&corrected))
	assertions.Equal(first, corrected)
	var progress string
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT value FROM archive_metadata WHERE key='synctech_sms.native_send_state_backfill_v1'`)).Scan(&progress))
	assertions.Equal(fmt.Sprint(expectedCursor), progress, "the cursor must advance with each committed bounded page")

	second, err := BackfillNativeSendStateContext(t.Context(), f.Store)
	requirements.NoError(err)
	assertions.Positive(second)
	assertions.Equal(totalUnsentCount, first+second)
	var stillSent int
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND message_type='sms' AND is_sent=TRUE`), source.ID).Scan(&stillSent))
	assertions.Equal(1, stillSent, "the native sent message remains sent")
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT value FROM archive_metadata WHERE key='synctech_sms.native_send_state_backfill_v1'`)).Scan(&progress))
	assertions.Equal("complete", progress)
	third, err := BackfillNativeSendStateContext(t.Context(), f.Store)
	requirements.NoError(err)
	assertions.Zero(third)
}
