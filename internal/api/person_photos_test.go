package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPersonPhotosDisabled(t *testing.T) {
	cfg := config.NewDefaultConfig()
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=example", nil)
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, request)
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
}

func TestPersonPhotosResolvesRetiredUIDAndDetectsConflict(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid, display_name) VALUES ('canonical', 'Example') RETURNING id`).Scan(&id))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_uid_aliases (retired_uid, surviving_person_id, reason) VALUES ('retired', ?, 'merge')`), id)
	require.NoError(err)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: "test", BaseURL: "https://contacts.example.test/dav", Username: "example", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/one/", DisplayName: "Example"}}})
	require.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_resources (address_book_id, href, remote_uid, remote_etag, remote_body, remote_semantic_hash, local_hash, mapping_status, governance, person_id) VALUES (?, ?, ?, ?, ?, ?, ?, 'mapped', 'remote', ?)`), books[0].ID, "https://contacts.example.test/books/one/person.vcf", "carddav-uid", `"one"`, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:carddav-uid\r\nFN:Example\r\nEND:VCARD\r\n"), "remote", "local", id)
	require.NoError(err)
	archive, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	conflict := false
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, archive, r.URL.Query().Get("archive_id"))
		if !conflict {
			http.NotFound(w, r)
			return
		}
		uid := r.URL.Query().Get("uid")
		person := "one"
		if uid == "retired" {
			person = "two"
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(docbankmedia.ExternalPersonResolution{
			PersonID: person, System: "msgvault", ArchiveID: archive,
			RequestedUID: uid, ResolvedUID: uid, UIDState: "current",
		}))
	}))
	defer remote.Close()
	client, err := docbankmedia.NewClient(remote.URL, nil)
	require.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: st, Destination: "test"}
	router := server.Router()
	get := func(uid string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid="+uid, nil))
		return response
	}
	response := get("retired")
	require.Equal(http.StatusOK, response.Code)
	require.Contains(response.Body.String(), `"uid":"canonical"`)
	require.Contains(response.Body.String(), `"link_state":"unlinked"`)
	response = get("carddav-uid")
	require.Equal(http.StatusOK, response.Code)
	require.Contains(response.Body.String(), `"uid":"canonical"`)
	conflict = true
	require.Equal(http.StatusConflict, get("retired").Code)
}

func TestPersonPhotosOnlyLiveConfirmedImages(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid, display_name) VALUES ('person', 'Example')`)
	require.NoError(err)
	const version = "00000000-0000-4000-8000-000000000001"
	hash := strings.Repeat("a", 64)
	for _, name := range []string{"live", "deleted"} {
		mid := f.CreateMessage(name)
		if name == "live" {
			require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "nonimage", Filename: "copy.pdf", MIMEType: "application/pdf", ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))
		}
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "image", Filename: name + ".jpg", MIMEType: "image/jpeg", ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))
		if name == "deleted" {
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), mid)
			require.NoError(err)
		}
	}
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash, MIMEType: "image/jpeg"}, store.DocbankAttachmentRef{NodeID: 7, VersionID: version}, ""))
	archive, err := f.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	withdrawDuringRead := false
	pageReads := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "resolve-external") {
			_, _ = fmt.Fprintf(w, `{"person_id":"p","system":"msgvault","archive_id":%q,"requested_uid":"person","resolved_uid":"person","uid_state":"current"}`, archive)
			return
		}
		pageReads++
		action := "assert"
		if withdrawDuringRead && pageReads > 1 {
			action = "withdrawn"
		}
		_, _ = fmt.Fprintf(w, `{"person_id":"p","state":"curated","revision":1,"assertions":[{"person_id":"p","assertion_id":"a","content_version_id":%q,"role":"depicted","action":%q},{"person_id":"p","assertion_id":"b","content_version_id":%q,"role":"depicted","action":"withdrawn"},{"person_id":"p","assertion_id":"replacement","content_version_id":"00000000-0000-4000-8000-000000000002","role":"depicted","action":"assert"}]}`, version, action, version)
	}))
	defer remote.Close()
	client, err := docbankmedia.NewClient(remote.URL, nil)
	require.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: f.Store, Destination: "attachments"}
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person", nil))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var result PersonPhotosResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(result.Photos, 1)
	require.Equal("a", result.Photos[0].AssertionID)
	require.Len(result.Photos[0].Occurrences, 1)
	require.Equal("live.jpg", result.Photos[0].Occurrences[0].Filename)
	withdrawDuringRead = true
	pageReads = 0
	response = httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person", nil))
	require.Equal(http.StatusConflict, response.Code)
}

func TestPersonPhotoLinkRevisionAndRetiredUID(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid, display_name) VALUES ('current', 'Example') RETURNING id`).Scan(&id))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_uid_aliases (retired_uid, surviving_person_id, reason) VALUES ('old', ?, 'merge')`), id)
	require.NoError(err)
	archive, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	stale := false
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		assert.Equal(t, `"7"`, r.Header.Get("If-Match"))
		var identity docbankmedia.ExternalPersonIdentity
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&identity))
		assert.Equal(t, archive, identity.ArchiveID)
		assert.Equal(t, "old", identity.UID)
		assert.Equal(t, "unlinked", identity.UIDState)
		if stale {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("ETag", `"8"`)
		_, _ = fmt.Fprintf(w, `{"system":"msgvault","archive_id":%q,"uid":"old","uid_state":"unlinked","uid_kind":"vcard_uid"}`, archive)
	}))
	defer remote.Close()
	client, err := docbankmedia.NewClient(remote.URL, nil)
	require.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: st, Destination: "attachments"}
	router := server.Router()
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/v1/people/by-uid/photo-link", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	body := `{"uid":"current","person_id":"p","revision":7,"state":"unlinked","linked_uid":"old"}`
	result := put(body)
	require.Equal(http.StatusOK, result.Code, result.Body.String())
	require.Contains(result.Body.String(), `"revision":8`)
	stale = true
	require.Equal(http.StatusPreconditionFailed, put(body).Code)
	require.Equal(http.StatusBadRequest, put(`{"uid":"current","person_id":"p","revision":7,"state":"unlinked","linked_uid":"elsewhere"}`).Code)
}
