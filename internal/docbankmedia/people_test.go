package docbankmedia

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveExternalPersonEscapesAndValidates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "a&b", r.URL.Query().Get("archive_id"))
		assert.Equal(t, "u+id", r.URL.Query().Get("uid"))
		_, _ = w.Write([]byte(`{"person_id":"p1","system":"msgvault","archive_id":"a&b","requested_uid":"u+id","resolved_uid":"u+id","uid_state":"current","forwarded":false}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, nil)
	require.NoError(t, err)
	resolved, err := client.ResolveExternalPerson(t.Context(), "a&b", "u+id")
	require.NoError(t, err)
	require.Equal(t, "p1", resolved.PersonID)
}

func TestPersonProtocolRejectsMalformedAndPreservesStatuses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{"malformed", 200, `{"person_id":"p","system":"msgvault","archive_id":"wrong","requested_uid":"u","resolved_uid":"u","uid_state":"current"}`, 0},
		{"not found", 404, `{}`, 404},
		{"conflict", 409, `{}`, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, nil)
			require.NoError(err)
			_, err = client.ResolveExternalPerson(t.Context(), "archive", "u")
			if tc.wantStatus == 0 {
				require.ErrorIs(err, ErrInvalidReceipt)
			} else {
				var remote *HTTPError
				require.ErrorAs(err, &remote)
				require.Equal(tc.wantStatus, remote.Status)
			}
		})
	}
}

func TestSetExternalPersonIdentityRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, `"4"`, r.Header.Get("If-Match"))
		assert.Equal(t, "/api/v1/people/by-id/person%2Fone/external-identities", r.URL.EscapedPath())
		w.Header().Set("ETag", `"5"`)
		_, _ = w.Write([]byte(`{"system":"msgvault","archive_id":"a","uid":"u","uid_state":"current","uid_kind":"vcard_uid"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, nil)
	require.NoError(t, err)
	_, revision, err := client.SetExternalPersonIdentity(t.Context(), "person/one", 4, ExternalPersonIdentity{System: "msgvault", ArchiveID: "a", UID: "u", UIDState: "current"})
	require.NoError(t, err)
	require.EqualValues(t, 5, revision)
}

func TestPersonAssertionsRejectsMismatchedPerson(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "100", r.URL.Query().Get("assertion_limit"))
		_, _ = w.Write([]byte(`{"person_id":"p","state":"active","revision":1,"assertions":[{"person_id":"other","assertion_id":"a","content_version_id":"v","role":"depicted","action":"assert"}]}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, nil)
	require.NoError(t, err)
	_, err = client.PersonAssertions(t.Context(), "p", "")
	require.ErrorIs(t, err, ErrInvalidReceipt)
}

func TestPersonAssertionsRejectsMissingPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"person_id":"p","state":"curated","revision":1}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, nil)
	require.NoError(t, err)
	_, err = client.PersonAssertions(t.Context(), "p", "")
	require.ErrorIs(t, err, ErrInvalidReceipt)
}
