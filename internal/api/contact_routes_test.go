package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/store"
)

func TestContactRoutesHTTPReadsAndErrors(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	srv, st := newIdentityLinkTestServer(t)
	pid := st.mustParticipant(t, "contact-http@example.test", "Avery Example", "example.test")
	p, _, err := st.CreatePersonFromParticipant(pid)
	requirements.NoError(err)
	paths := []struct {
		path string
		want int
	}{
		{"/api/v1/people/contact-candidates?query=Avery", http.StatusOK},
		{"/api/v1/people/messaging-routes?person_uid=" + p.VCardUID, http.StatusOK},
		{"/api/v1/people/contact-candidates?query=%25_", http.StatusOK},
		{"/api/v1/people/contact-candidates?query=", http.StatusBadRequest},
		{"/api/v1/people/contact-candidates?query=Avery&query=Other", http.StatusBadRequest},
		{"/api/v1/people/messaging-routes?person_uid=" + p.VCardUID + "&person_uid=other", http.StatusBadRequest},
		{"/api/v1/people/messaging-routes?person_uid=" + p.VCardUID + "&network=whatsapp&network=matrix", http.StatusBadRequest},
		{"/api/v1/people/contact-candidates?query=Avery&limit=101", http.StatusBadRequest},
		{"/api/v1/people/contact-candidates?query=Avery&after_id=-1", http.StatusBadRequest},
		{"/api/v1/people/contact-candidates?query=Avery&limit=garbage", http.StatusBadRequest},
		{"/api/v1/people/messaging-routes?person_uid=unknown", http.StatusNotFound},
		{"/api/v1/people/messaging-routes?person_uid=" + p.VCardUID + "&after_observation_id=-1", http.StatusBadRequest},
	}
	for _, tt := range paths {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))
		assertions.Equal(tt.want, w.Code, tt.path+" "+w.Body.String())
		if tt.want == http.StatusOK {
			assertions.Equal("no-store", w.Header().Get("Cache-Control"))
		}
	}
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, paths[0].path, nil))
	var page store.ContactCandidatePage
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
	requirements.Len(page.Candidates, 1)
	assertions.Equal(p.VCardUID, page.Candidates[0].PersonUID)
	_, err = st.RetirePersonUIDAliasContext(t.Context(), "http-gone", nil, "deleted")
	requirements.NoError(err)
	gone := httptest.NewRecorder()
	srv.Router().ServeHTTP(gone, httptest.NewRequest(http.MethodGet, "/api/v1/people/messaging-routes?person_uid=http-gone", nil))
	assertions.Equal(http.StatusGone, gone.Code)
}

func TestContactRoutesHTTPDatabaseFailureIsNotNoMatches(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	srv, st := newIdentityLinkTestServer(t)
	requirements.NoError(st.Close())
	for _, path := range []string{"/api/v1/people/contact-candidates?query=Avery", "/api/v1/people/messaging-routes?person_uid=synthetic"} {
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assertions.Equal(http.StatusServiceUnavailable, w.Code)
		assertions.Contains(w.Body.String(), "contact_lookup_unavailable")
		assertions.NotContains(w.Body.String(), `"candidates":[]`)
	}
}

func TestContactRoutesHTTPRequiresOwnerAndAvailableBackend(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)
	_, secret, _, err := reg.Issue("contact-lookup", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "owner@example.test"}}, time.Time{})
	require.NoError(t, err)
	for _, path := range []string{"/api/v1/people/contact-candidates?query=Avery", "/api/v1/people/messaging-routes?person_uid=synthetic"} {
		for _, test := range []struct {
			name, header, value string
			status              int
		}{
			{"anonymous", "", "", http.StatusUnauthorized},
			{"delegated", apiprotocol.AgentTokenHeader, secret, http.StatusUnauthorized},
			{"owner unavailable", "Authorization", "Bearer owner-key", http.StatusServiceUnavailable},
		} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "192.0.2.2:4242"
			if test.header != "" {
				req.Header.Set(test.header, test.value)
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assert.Equal(t, test.status, w.Code, test.name+" "+w.Body.String())
		}
	}
}
