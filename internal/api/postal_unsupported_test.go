package api

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPostalUnsupportedSnapshotDoesNotHideOtherAddresses(t *testing.T) {
	srv, wrapped := newIdentityLinkTestServer(t)
	id := seedAPIPerson(t, wrapped.Store)
	bad, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: new("12 Example Street"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(t, err)
	_, err = wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: new("34 Example Street"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
	require.NoError(t, err)
	_, err = wrapped.DB().ExecContext(t.Context(), wrapped.Rebind(`UPDATE person_addresses SET original_value = ? WHERE id = ?`), "msgvault:postal-input:v2\n{}", bad.Envelope.ID)
	require.NoError(t, err)
	for _, path := range []string{personProfilePath(id), personProfilePath(id) + "/history"} {
		t.Run(path, func(t *testing.T) {
			response := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var profile struct {
				Addresses []store.PersonAddress `json:"addresses"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &profile))
			require.Len(t, profile.Addresses, 2)
			for _, address := range profile.Addresses {
				require.NotNil(t, address.StreetAddress)
				if address.Envelope.ID == bad.Envelope.ID {
					assert.Equal(t, "12 Example Street", *address.StreetAddress)
					assert.Empty(t, address.OriginalValue)
				} else {
					assert.Equal(t, "34 Example Street", *address.StreetAddress)
				}
			}
		})
	}
}
