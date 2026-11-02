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
			_testAssert := assert.New(t)
			_testRequire := require.New(t)

			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("x", "1001")
			_testRequire.NoError(err)
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
			_testRequire.NoError(err)
			pid, err := RecordAuthor(t.Context(), st, source.ID, private, at)
			_testRequire.NoError(err)
			unknown, err := client.Me(t.Context())
			_testRequire.NoError(err)
			_testAssert.False(unknown.ProtectedKnown)
			next, err := RecordAuthor(t.Context(), st, source.ID, unknown, at.Add(time.Hour))
			_testRequire.NoError(err)
			_testAssert.Equal(pid, next)
			observations, err := st.ListParticipantObservationsContext(t.Context(), pid, true)
			_testRequire.NoError(err)
			_testRequire.Len(observations, 1)
			_testAssert.Equal(store.ContactAddressProviderIdentity, observations[0].AddressKind)
			_testAssert.Contains(observations[0].Envelope.TypeTokens, "x_author_protected")
			var label string
			_testRequire.NoError(st.DB().QueryRow(st.Rebind("SELECT COALESCE(display_name,'') FROM participants WHERE id=?"), pid).Scan(&label))
			_testAssert.Empty(label)
		})
	}
}
