package importer

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestEmlxReceiptStrictIdentity(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	id := strings.Repeat("a", 64) + "/Messages/1.emlx"
	receipt := emlxReceipt{Version: 1, ID: id, Signature: strings.Repeat("b", 64), Target: "emlx-" + strings.Repeat("c", 64), RFCID: "synthetic@example.test"}
	encoded, err := encodeEmlxReceipt(receipt)
	r.NoError(err)
	got, ok := decodeEmlxReceipt(encoded, id)
	r.True(ok)
	a.Equal(receipt, got)
	for _, bad := range []string{encoded + "x", strings.Replace(encoded, `"version":1`, `"version":2`, 1), strings.Replace(encoded, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(encoded, `"version":1`, `"unknown":1,"version":1`, 1), `{}`} {
		_, ok = decodeEmlxReceipt(bad, id)
		a.False(ok, bad)
	}
	_, ok = decodeEmlxReceipt(encoded, strings.Repeat("d", 64)+"/Messages/1.emlx")
	a.False(ok)
}

func FuzzEmlxReceiptRoundTrip(f *testing.F) {
	f.Add("Messages/1.emlx", "synthetic@example.test", "")
	f.Add("Archive.mbox/子/2.emlx", "id@example.test", "parent@example.test")
	f.Fuzz(func(t *testing.T, rel, rfc, parent string) {
		// Bound materialized strings while keeping every drawn byte class.
		rel = "Messages/" + base64.RawURLEncoding.EncodeToString([]byte(rel)[:min(len(rel), 512)]) + ".emlx"
		rfc = strings.ToValidUTF8(rfc[:min(len(rfc), 1024)], "�")
		parent = strings.ToValidUTF8(parent[:min(len(parent), 1024)], "�")
		receipt := emlxReceipt{Version: 1, ID: strings.Repeat("a", 64) + "/" + rel, Signature: strings.Repeat("b", 64), Target: "emlx-" + strings.Repeat("c", 64), RFCID: rfc, Reply: parent}
		encoded, err := encodeEmlxReceipt(receipt)
		require.NoError(t, err)
		decoded, ok := decodeEmlxReceipt(encoded, receipt.ID)
		require.True(t, ok)
		assert.Equal(t, receipt, decoded)
	})
}

func countEmlxLedgerEntries(t *testing.T, st *store.Store, sourceID int64, provider, status string) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM source_import_items
 WHERE source_id = ? AND provider = ? AND status = ?`), sourceID, provider, status).Scan(&count))
	return count
}
