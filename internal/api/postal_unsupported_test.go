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

func TestPostalLegacyAndUnsupportedSnapshotReplacement(t *testing.T) {
	t.Run("unsupported snapshot", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		stored := "msgvault:postal-input:v2\n{}"
		srv, wrapped := newIdentityLinkTestServer(t)
		id := seedAPIPerson(t, wrapped.Store)
		bad, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: new("12 Example Street"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		require.NoError(err)
		_, err = wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: new("34 Example Street"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		require.NoError(err)
		_, err = wrapped.DB().ExecContext(t.Context(), wrapped.Rebind(`UPDATE person_addresses SET original_value = ? WHERE id = ?`), stored, bad.Envelope.ID)
		require.NoError(err)
		for _, path := range []string{personProfilePath(id), personProfilePath(id) + "/history"} {
			response := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var result struct {
				Addresses []store.PersonAddress `json:"addresses"`
			}
			require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
			require.Len(result.Addresses, 2)
			for _, address := range result.Addresses {
				require.NotNil(address.StreetAddress)
				if address.Envelope.ID == bad.Envelope.ID {
					assert.Empty(address.OriginalValue)
					assert.Equal("12 Example Street", *address.StreetAddress)
				} else {
					assert.Equal("34 Example Street", *address.StreetAddress)
				}
			}
		}
		read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
		body := fmt.Appendf(nil, `{"addresses":{"supersede":[%d],"add":[{"address_kind":"postal","street_address":"Updated","original_input":{"address_kind":"postal"},"envelope":{"source":"user"}}]}}`, bad.Envelope.ID)
		response := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
		assert.Equal(http.StatusConflict, response.Code, response.Body.String())
		assert.Contains(response.Body.String(), "unsupported_postal_snapshot")
		rows, err := wrapped.ListPersonAddressesContext(t.Context(), id, true)
		require.NoError(err)
		require.Len(rows, 2)
		var found bool
		for _, address := range rows {
			if address.Envelope.ID == bad.Envelope.ID {
				found = true
				assert.Equal(stored, address.OriginalValue)
			}
		}
		assert.True(found)
	})
	t.Run("legacy text", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		original := "12 Example Street; Unit 7"
		srv, wrapped := newIdentityLinkTestServer(t)
		id := seedAPIPerson(t, wrapped.Store)
		old, err := wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, OriginalValue: original, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
		require.NoError(err)
		for _, path := range []string{personProfilePath(id), personProfilePath(id) + "/history"} {
			response := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
			require.Equal(http.StatusOK, response.Code, response.Body.String())
			var profile struct {
				Addresses []store.PersonAddress `json:"addresses"`
			}
			require.NoError(json.Unmarshal(response.Body.Bytes(), &profile))
			require.Len(profile.Addresses, 1)
			require.NotNil(profile.Addresses[0].FreeText)
			assert.Equal(original, *profile.Addresses[0].FreeText)
		}
		read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
		body := fmt.Appendf(nil, `{"addresses":{"supersede":[%d],"add":[{"address_kind":"postal","free_text":%q,"country_name":"Canada","original_input":{"address_kind":"postal","original_value":%q},"envelope":{"source":"user"}}]}}`, old.Envelope.ID, original, original)
		saved := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
		require.Equal(http.StatusOK, saved.Code, saved.Body.String())
		var profile store.PersonProfile
		require.NoError(json.Unmarshal(saved.Body.Bytes(), &profile))
		require.Len(profile.Addresses, 1)
		require.NotNil(profile.Addresses[0].FreeText)
		assert.Equal(original, *profile.Addresses[0].FreeText)
		assert.Equal("Canada", *profile.Addresses[0].CountryName)
	})
}
