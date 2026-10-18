package daemonclient

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetScopedCardDAVPublicationReceiptUsesOwnerLookupRoute(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	receiptID := "018f4f7a-9b2c-7c13-9f0d-7c205e000011"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal(http.MethodGet, r.Method)
		assertions.Equal("/api/v1/carddav/scoped/publication-receipts/"+receiptID, r.URL.Path)
		assertions.Equal("synthetic-owner-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"receipt":{"account_id":7,"address_book_id":9,"body_sha256":"synthetic-digest","created_at":"2026-10-18T00:00:00Z","id":"` + receiptID + `","person_id":11,"person_uid":"synthetic-person-uid","remote_etag":"\"synthetic-etag\"","state":"verified"}}`))
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { assertions.NoError(client.Close()) })

	receipt, err := client.GetScopedCardDAVPublicationReceipt(t.Context(), receiptID)
	requirements.NoError(err)
	requirements.NotNil(receipt)
	requirements.NotNil(receipt.Receipt)
	assertions.Equal(receiptID, receipt.Receipt.ID)
	assertions.Equal("verified", receipt.Receipt.State)
}
