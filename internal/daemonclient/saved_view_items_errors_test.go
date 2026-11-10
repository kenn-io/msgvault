package daemonclient

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
)

func TestSavedViewItemsRejectionDetails(t *testing.T) {
	err := handleErrorBody(http.StatusUnprocessableEntity, []byte(`{"error":"saved_view_items_rejected","message":"Entry keys rejected","rejected":[{"entry_key":"source:1:message:missing","reason":"not_found"}]}`))
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, "saved_view_items_rejected", apiErr.Code)
	assert.Equal(t, []EntryKeyRejection{{EntryKey: "source:1:message:missing", Reason: "not_found"}}, apiErr.Rejected)
}
