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
	for _, tc := range []struct {
		name        string
		unsupported bool
	}{
		{"legacy text", false},
		{"unsupported snapshot", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			srv, wrapped := newIdentityLinkTestServer(t)
			id := seedAPIPerson(t, wrapped.Store)
			original := "12 Example Street; Unit 7"
			input := store.PersonAddressInput{AddressKind: store.PersonAddressPostal, OriginalValue: original, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}}
			if tc.unsupported {
				input.OriginalValue = ""
				input.StreetAddress = new("12 Example Street")
			}
			old, err := wrapped.AddPersonAddressContext(t.Context(), id, input)
			require.NoError(err)
			if tc.unsupported {
				original = "msgvault:postal-input:v2\n{}"
				_, err = wrapped.AddPersonAddressContext(t.Context(), id, store.PersonAddressInput{AddressKind: store.PersonAddressPostal, StreetAddress: new("34 Example Street"), Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}})
				require.NoError(err)
				_, err = wrapped.DB().ExecContext(t.Context(), wrapped.Rebind(`UPDATE person_addresses SET original_value = ? WHERE id = ?`), original, old.Envelope.ID)
				require.NoError(err)
			}
			for _, path := range []string{personProfilePath(id), personProfilePath(id) + "/history"} {
				response := doRequest(t, srv.Router(), http.MethodGet, path, nil, nil)
				require.Equal(http.StatusOK, response.Code, response.Body.String())
				var result struct {
					Addresses []store.PersonAddress `json:"addresses"`
				}
				require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
				if tc.unsupported {
					require.Len(result.Addresses, 2)
					for _, address := range result.Addresses {
						if address.Envelope.ID == old.Envelope.ID {
							assert.Empty(address.OriginalValue)
							require.NotNil(address.StreetAddress)
							assert.Equal("12 Example Street", *address.StreetAddress)
						} else {
							assert.Equal("34 Example Street", *address.StreetAddress)
						}
					}
				} else {
					require.Len(result.Addresses, 1)
					assert.Equal(original, *result.Addresses[0].FreeText)
				}
			}
			read := doRequest(t, srv.Router(), http.MethodGet, personProfilePath(id), nil, nil)
			body := fmt.Appendf(nil, `{"addresses":{"supersede":[%d],"add":[{"address_kind":"postal","street_address":"Updated","original_input":{"address_kind":"postal"},"envelope":{"source":"user"}}]}}`, old.Envelope.ID)
			if !tc.unsupported {
				body = fmt.Appendf(nil, `{"addresses":{"supersede":[%d],"add":[{"address_kind":"postal","free_text":%q,"country_name":"Canada","original_input":{"address_kind":"postal","original_value":%q},"envelope":{"source":"user"}}]}}`, old.Envelope.ID, original, original)
			}
			response := doRequest(t, srv.Router(), http.MethodPatch, personProfilePath(id), body, map[string]string{"If-Match": read.Header().Get("ETag")})
			if tc.unsupported {
				assert.Equal(http.StatusConflict, response.Code, response.Body.String())
				assert.Contains(response.Body.String(), "unsupported_postal_snapshot")
				rows, err := wrapped.ListPersonAddressesContext(t.Context(), id, true)
				require.NoError(err)
				require.Len(rows, 2)
				var found bool
				for _, address := range rows {
					if address.Envelope.ID == old.Envelope.ID {
						found = true
						assert.Equal(original, address.OriginalValue)
					}
				}
				assert.True(found)
			} else {
				require.Equal(http.StatusOK, response.Code, response.Body.String())
				var profile store.PersonProfile
				require.NoError(json.Unmarshal(response.Body.Bytes(), &profile))
				require.Len(profile.Addresses, 1)
				assert.Equal(original, *profile.Addresses[0].FreeText)
			}
		})
	}
}
