package x

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestXUnknownProtectionDoesNotReopenAuthor(t *testing.T) {
	for _, field := range []string{"", `,"protected":null`} {
		t.Run("later"+field, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "1001")
			require.NoError(t, err)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body := `{"id":"1002","name":"Private Author","username":"private_author","protected":true}`
				if calls > 1 {
					body = `{"id":"1002","name":"Unverified Name","username":"unverified_handle"` + field + `}`
				}
				_, err := fmt.Fprint(w, `{"data":`+body+`}`)
				assert.NoError(t, err)
			}))
			defer server.Close()
			client := NewClient(server.URL, server.Client())
			at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
			private, err := client.Me(t.Context())
			require.NoError(t, err)
			pid, err := RecordAuthor(t.Context(), st, source.ID, private, at)
			require.NoError(t, err)
			unknown, err := client.Me(t.Context())
			require.NoError(t, err)
			assert.False(t, unknown.ProtectedKnown)
			next, err := RecordAuthor(t.Context(), st, source.ID, unknown, at.Add(time.Hour))
			require.NoError(t, err)
			assert.Equal(t, pid, next)
			observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
			require.NoError(t, err)
			require.Len(t, observations, 1)
			assert.Equal(t, store.ContactAddressProviderIdentity, observations[0].AddressKind)
			assert.Contains(t, observations[0].Envelope.TypeTokens, "x_author_protected")
			var label string
			require.NoError(t, st.DB().QueryRow(st.Rebind("SELECT COALESCE(display_name,'') FROM participants WHERE id=?"), pid).Scan(&label))
			assert.Empty(t, label)
		})
	}
}
