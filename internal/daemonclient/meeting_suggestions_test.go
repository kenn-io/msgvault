package daemonclient_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestSuggestMeetingLinksDefaultLimit(t *testing.T) {
	for _, limit := range []int{0, 5} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				assert.Equal(t, "msg:42", req.URL.Query().Get("ref"))
				if limit == 0 {
					assert.False(t, req.URL.Query().Has("limit"))
				} else {
					assert.Equal(t, "5", req.URL.Query().Get("limit"))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"suggestions":[],"unexpandable":[]}`))
			}))
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			_, err = client.SuggestMeetingLinks(t.Context(), "msg:42", limit)
			require.NoError(t, err)
		})
	}
}
