package x

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientProtectionPresence(t *testing.T) {
	for _, tc := range []struct {
		name, field               string
		known, protected, invalid bool
	}{
		{name: "omitted"},
		{name: "null", field: `,"protected":null`},
		{name: "public", field: `,"protected":false`, known: true},
		{name: "protected", field: `,"protected":true`, known: true, protected: true},
		{name: "string", field: `,"protected":"false"`, invalid: true},
		{name: "number", field: `,"protected":0`, invalid: true},
	} {
		for _, endpoint := range []string{"me", "timeline"} {
			t.Run(tc.name+"/"+endpoint, func(t *testing.T) {
				author := `{"id":"1001","name":"Example Author","username":"example_author"` + tc.field + `}`
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if endpoint == "me" {
						_, err := fmt.Fprint(w, `{"data":`+author+`}`)
						assert.NoError(t, err)
						return
					}
					_, err := fmt.Fprint(w, `{"data":[`+rootJSON+`],"includes":{"users":[`+author+`]}}`)
					assert.NoError(t, err)
				}))
				defer server.Close()
				client := NewClient(server.URL, server.Client())
				var user User
				var err error
				if endpoint == "me" {
					user, err = client.Me(t.Context())
				} else {
					var page Page
					page, err = client.Timeline(t.Context(), TimelineRequest{OwnerID: "1001", MaxResults: 5})
					if err == nil {
						require.Len(t, page.Users, 1)
						user = page.Users[0]
					}
				}
				if tc.invalid {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, tc.known, user.ProtectedKnown)
				assert.Equal(t, tc.protected, user.Protected)
			})
		}
	}
}
