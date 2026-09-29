package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestPersonCreateDisplayName(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		want        *string
	}{
		{name: "omitted", want: new("Alex Example")},
		{name: "null", field: `,"display_name":null`, want: new("Alex Example")},
		{name: "explicit", field: `,"display_name":"  Custom Example  "`, want: new("Custom Example")},
		{name: "empty", field: `,"display_name":""`},
		{name: "blank", field: `,"display_name":" \t\n "`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			srv, st := newIdentityLinkTestServer(t)
			id := st.mustParticipant(t, "promotion@example.com", "Alex Example", "example.com")
			response := personRequest(t, srv, http.MethodPost, peoplePath,
				fmt.Appendf(nil, `{"participant_id":%d%s}`, id, tc.field), "")
			require.Equal(http.StatusCreated, response.Code, response.Body.String())
			var person store.Person
			require.NoError(json.Unmarshal(response.Body.Bytes(), &person))
			assert.Equal(tc.want, person.DisplayName)
			assert.Equal(int64(1), person.Revision)
			assert.Equal(fmt.Sprintf(`"person-%d-r1"`, person.ID), response.Header().Get("ETag"))
			detail := personRequest(t, srv, http.MethodGet, fmt.Sprintf("%s/%d", peoplePath, person.ID), nil, "")
			require.Equal(http.StatusOK, detail.Code)
			var got store.Person
			require.NoError(json.Unmarshal(detail.Body.Bytes(), &got))
			assert.Equal(tc.want, got.DisplayName)
			directory := personRequest(t, srv, http.MethodGet, peoplePath+"/directory", nil, "")
			require.Equal(http.StatusOK, directory.Code, directory.Body.String())
			var page DirectoryPeopleResponse
			require.NoError(json.Unmarshal(directory.Body.Bytes(), &page))
			require.Len(page.People, 1)
			assert.Equal(tc.want, page.People[0].DisplayName)
			repeated := personRequest(t, srv, http.MethodPost, peoplePath,
				fmt.Appendf(nil, `{"participant_id":%d,"display_name":"Replacement"}`, id), "")
			require.Equal(http.StatusOK, repeated.Code, repeated.Body.String())
			var existing store.Person
			require.NoError(json.Unmarshal(repeated.Body.Bytes(), &existing))
			assert.Equal(person, existing)
			assert.Equal(response.Header().Get("ETag"), repeated.Header().Get("ETag"))
		})
	}
}

func TestPersonCreateSchemaOptionalNullableDisplayName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, doc := range []*huma.OpenAPI{OpenAPIDocument(), openAPIClientDocument()} {
		schema := doc.Components.Schemas.Map()["CreatePersonRequest"]
		require.NotNil(schema)
		assert.Contains(schema.Required, "participant_id")
		assert.NotContains(schema.Required, "display_name")
		require.Contains(schema.Properties, "display_name")
		assert.True(schema.Properties["display_name"].Nullable)
	}
}

// An explicit string must never fall back to an observed name, even when
// normalization clears it. JSON serialization supplies arbitrary valid wire
// strings (including escaped controls) to the real API and SQLite store.
func FuzzPersonCreateExplicitDisplayName(f *testing.F) {
	for _, seed := range []string{"", "\t\n", " Custom Example ", "例示", "\x00", "\xff"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		assert := assert.New(t)
		require := require.New(t)
		st := testutil.NewSQLiteTestStore(t)
		srv := NewServer(&config.Config{}, &stubIdentityCacheStore{Store: st}, nil, testLogger())
		id, err := st.EnsureParticipant("promotion@example.com", "Observed Example", "example.com")
		require.NoError(err)
		body, err := json.Marshal(map[string]any{"participant_id": id, "display_name": name})
		require.NoError(err)
		response := personRequest(t, srv, http.MethodPost, peoplePath, body, "")
		require.Equal(http.StatusCreated, response.Code, response.Body.String())
		var got store.Person
		require.NoError(json.Unmarshal(response.Body.Bytes(), &got))
		// encoding/json replaces invalid UTF-8 at the wire boundary.
		var wire struct {
			DisplayName string `json:"display_name"`
		}
		require.NoError(json.Unmarshal(body, &wire))
		want := strings.TrimSpace(wire.DisplayName)
		if want == "" {
			assert.Nil(got.DisplayName)
		} else {
			assert.Equal(&want, got.DisplayName)
		}
	})
}

func TestPersonCreateGeneratedBodyNamePresence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// Marshal actual generated create bodies, then exercise the real API.
	srv, st := newIdentityLinkTestServer(t)
	for i, name := range []*string{nil, new("")} {
		id := st.mustParticipant(t, fmt.Sprintf("generated-%d@example.com", i), "Alex Example", "example.com")
		body, err := json.Marshal(generated.CreatePersonBody{ParticipantID: id, DisplayName: name})
		require.NoError(err)
		var fields map[string]json.RawMessage
		require.NoError(json.Unmarshal(body, &fields))
		if name == nil {
			assert.NotContains(fields, "display_name")
		} else {
			assert.Equal(`""`, string(fields["display_name"]))
		}
		response := personRequest(t, srv, http.MethodPost, peoplePath, body, "")
		require.Equal(http.StatusCreated, response.Code, response.Body.String())
		var got store.Person
		require.NoError(json.Unmarshal(response.Body.Bytes(), &got))
		if name == nil {
			assert.Equal(new("Alex Example"), got.DisplayName)
		} else {
			assert.Nil(got.DisplayName)
		}
	}
}
