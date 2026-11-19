package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleCLISyncLabelRefresh(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		query string
		full  bool
		valid bool
		value bool
	}{
		{query: "email=reader@example.test&refresh_labels=true", full: true, valid: true, value: true},
		{query: "source_id=42&refresh_labels=true&limit=0&noresume=true", full: true, valid: true, value: true},
		{query: "refresh_labels=false", full: true, valid: true},
		{query: "refresh_labels=false&query=in:inbox", full: true, valid: true},
		{query: "refresh_labels=false", valid: true},
		{query: "refresh_labels=true", full: true},
		{query: "email=reader@example.test&refresh_labels=true"},
		{query: "email=reader@example.test&refresh_labels=maybe", full: true},
		{query: "email=reader@example.test&refresh_labels=", full: true},
		{query: "email=reader@example.test&refresh_labels=true&refresh_labels=false", full: true},
		{query: "email=reader@example.test&refresh_labels=false&refresh_labels=false", full: true},
		{query: "email=reader@example.test&refresh_labels=true&query=", full: true},
		{query: "email=reader@example.test&refresh_labels=true&after=", full: true},
		{query: "email=reader@example.test&refresh_labels=true&before=2026-01-01", full: true},
		{query: "email=reader@example.test&refresh_labels=true&folder=", full: true},
		{query: "email=reader@example.test&refresh_labels=true&skip-folder=Trash", full: true},
		{query: "email=reader@example.test&refresh_labels=true&limit=1", full: true},
		{query: "email=reader@example.test&refresh_labels=true&limit=", full: true},
		{query: "email=reader@example.test&refresh_labels=true&limit=0&limit=1", full: true},
		{query: "email=reader@example.test&email=other@example.test&refresh_labels=true", full: true},
		{query: "email=reader@example.test&source_id=42&refresh_labels=true", full: true},
		{query: "email=&source_id=42&refresh_labels=true", full: true},
		{query: "source_id=42&source_id=43&refresh_labels=true", full: true},
	} {
		t.Run(fmt.Sprintf("%s/full=%t", scenario.query, scenario.full), func(t *testing.T) {
			t.Parallel()
			calls := 0
			var got CLISyncRequest
			srv := newCLIHandlerTestServer(&mockStore{
				syncFunc: func(_ context.Context, req CLISyncRequest, _ func(CLISyncEvent) error) error {
					calls++
					got = req
					return nil
				},
			})
			path := "/api/v1/cli/sync"
			if scenario.full {
				path += "-full"
			}
			resp := servePOSTTestRequest(t.Context(), srv, path+"?"+scenario.query)
			if !scenario.valid {
				assert.Equal(t, http.StatusBadRequest, resp.Code)
				assert.Zero(t, calls, "invalid repair must not start a runner")
				return
			}
			requireNDJSONResponse(t, resp)
			require.Equal(t, 1, calls)
			assert.Equal(t, scenario.value, got.RefreshLabels)
			assert.Equal(t, scenario.full, got.Full)
		})
	}
}
