package importer

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestEmlxReceiptStrictIdentity(t *testing.T) {
	r, a := require.New(t), assert.New(t)
	id := strings.Repeat("a", 64) + "/Messages/1.emlx"
	receipt := emlxReceipt{Version: 1, ID: id, Signature: strings.Repeat("b", 64), SourceParts: map[string]string{"2": strings.Repeat("d", 64)}, Target: "emlx-" + strings.Repeat("c", 64)}
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

// FuzzEmlxReceiptDecode feeds arbitrary ledger text to the decoder. Anything
// it accepts must satisfy every receipt rule and re-encode to stable text.
func FuzzEmlxReceiptDecode(f *testing.F) {
	id := strings.Repeat("a", 64) + "/Messages/1.emlx"
	valid, err := encodeEmlxReceipt(emlxReceipt{
		Version: 1, ID: id, Signature: strings.Repeat("b", 64),
		SourceParts: map[string]string{"2": strings.Repeat("d", 64)}, Target: "emlx-" + strings.Repeat("c", 64),
	})
	require.NoError(f, err)
	f.Add(valid)
	f.Add(strings.Replace(valid, `"2"`, `"02"`, 1))
	f.Add(strings.Replace(valid, `"2"`, `"0"`, 1))
	f.Add(strings.Replace(valid, `"signature":"`, `"signature":"x`, 1))
	f.Add(`{"version":1,"id":"` + id + `","signature":"","source_parts":null,"target":"emlx-` +
		strings.Repeat("c", 64) + `"}`)
	f.Add(`{}`)
	f.Fuzz(func(t *testing.T, text string) {
		receipt, ok := decodeEmlxReceipt(text, id)
		if !ok {
			return
		}
		assert.Equal(t, emlxReceiptVersion, receipt.Version)
		assert.Equal(t, id, receipt.ID)
		assert.True(t, receipt.Signature == "" || store.IsEmlxDigest(receipt.Signature))
		assert.True(t, store.IsEmlxTargetID(receipt.Target))
		for key, hash := range receipt.SourceParts {
			n, err := strconv.Atoi(key)
			require.NoError(t, err)
			assert.Positive(t, n)
			assert.Equal(t, strconv.Itoa(n), key)
			assert.True(t, store.IsEmlxDigest(hash))
		}
		encoded, err := encodeEmlxReceipt(receipt)
		require.NoError(t, err)
		again, ok := decodeEmlxReceipt(encoded, id)
		require.True(t, ok)
		reencoded, err := encodeEmlxReceipt(again)
		require.NoError(t, err)
		assert.Equal(t, encoded, reencoded)
	})
}

func countEmlxLedgerEntries(t *testing.T, st *store.Store, sourceID int64, provider, status string) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM source_import_items
 WHERE source_id = ? AND provider = ? AND status = ?`), sourceID, provider, status).Scan(&count))
	return count
}
