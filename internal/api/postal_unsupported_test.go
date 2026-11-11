package api

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPostalUnsupportedSnapshotReadAndReplacement(t *testing.T) {
	for _, tc := range []struct{ name, stored string }{
		{"new version", "msgvault:postal-input:v2\n{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, wrapped := newIdentityLinkTestServer(t)
			id := seedAPIPerson(t, wrapped.Store)
			bad, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: new("12 Example Street"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
			require.NoError(t, err)
			_, err = wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: new("34 Example Street"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
			require.NoError(t, err)
			_, err = wrapped.DB().ExecContext(t.Context(), wrapped.Rebind(`UPDATE person_addresses SET original_value = ? WHERE id = ?`), tc.stored, bad.Envelope.ID)
			require.NoError(t, err)
			for _, path := range []string{personProfilePath(id), personProfilePath(id) + "/history"} {
				response := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				var result struct {
					Addresses []store.PersonAddress `json:"addresses"`
				}
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
				require.Len(t, result.Addresses, 2)
				for _, address := range result.Addresses {
					require.NotNil(t, address.StreetAddress)
					if address.Envelope.ID == bad.Envelope.ID {
						assert.Empty(t, address.OriginalValue)
						assert.Equal(t, "12 Example Street", *address.StreetAddress)
					} else {
						assert.Equal(t, "34 Example Street", *address.StreetAddress)
					}
				}
			}
			read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
			body := fmt.Appendf(nil, `{"addresses":{"supersede":[%d],"add":[{"address_kind":"postal","street_address":"Updated","original_input":{"address_kind":"postal"},"envelope":{"source":"user"}}]}}`, bad.Envelope.ID)
			response := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
			assert.Equal(t, http.StatusConflict, response.Code, response.Body.String())
			assert.Contains(t, response.Body.String(), "unsupported_postal_snapshot")
			rows, err := wrapped.ListPersonAddressesContext(t.Context(), id, true)
			require.NoError(t, err)
			require.Len(t, rows, 2)
			var found bool
			for _, address := range rows {
				if address.Envelope.ID == bad.Envelope.ID {
					found = true
					assert.Equal(t, tc.stored, address.OriginalValue)
				}
			}
			assert.True(t, found)
		})
	}
}
