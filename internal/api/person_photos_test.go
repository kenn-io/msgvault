package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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
	for i := range 64 {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_uid_aliases (retired_uid, surviving_person_id, reason) VALUES (?, ?, 'merge')`), fmt.Sprintf("alias-%d", i), id)
		require.NoError(err)
	}
	archive, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	conflict := false
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, archive, r.URL.Query().Get("archive_id"))
		if !conflict {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found"}`))
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
	for _, name := range []string{"live", "generic", "deleted"} {
		mid := f.CreateMessage(name)
		if name == "live" {
			require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "nonimage", Filename: "copy.pdf", MIMEType: "application/pdf", ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))
		}
		mime := "image/jpeg"
		if name == "generic" {
			mime = "application/octet-stream"
		}
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "image", Filename: name + ".jpg", MIMEType: mime, ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))
		if name == "deleted" {
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), mid)
			require.NoError(err)
		}
	}
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash, MIMEType: "image/jpeg"}, store.DocbankAttachmentRef{NodeID: 7, VersionID: version}, ""))
	archive, err := f.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "resolve-external") {
			_, _ = fmt.Fprintf(w, `{"person_id":"p","system":"msgvault","archive_id":%q,"requested_uid":"person","resolved_uid":"person","uid_state":"current"}`, archive)
			return
		}
		action := "assert"
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
	require.Contains(response.Body.String(), `"excerpt":""`, "face-photo occurrences keep the required excerpt key even without text")
	var result PersonPhotosResponse
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	require.Len(result.Photos, 1)
	require.Equal("a", result.Photos[0].AssertionID)
	require.Len(result.Photos[0].Occurrences, 2)
	require.ElementsMatch([]string{"copy.pdf", "generic.jpg"}, []string{result.Photos[0].Occurrences[0].Filename, result.Photos[0].Occurrences[1].Filename})

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

func TestPersonPhotosOccurrencePages(t *testing.T) {
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('person')`)
	require.NoError(t, err)
	const version = "00000000-0000-4000-8000-000000000001"
	const version2 = "00000000-0000-4000-8000-000000000002"
	hash := strings.Repeat("b", 64)
	hash2 := strings.Repeat("c", 64)
	for i := range 11 {
		mid := f.CreateMessage(fmt.Sprintf("message-%d", i))
		require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "file", Filename: "photo", ContentHash: hash, StoragePath: "bb/" + hash, State: "stored"}))
		if i < 3 {
			require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "other", Filename: "other", ContentHash: hash2, StoragePath: "cc/" + hash2, State: "stored"}))
		}
		require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "duplicate", Filename: "duplicate", ContentHash: hash, StoragePath: "bb/" + hash, State: "stored"}))
	}
	require.NoError(t, f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash2}, store.DocbankAttachmentRef{NodeID: 2, VersionID: version2}, ""))
	require.NoError(t, f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: 1, VersionID: version}, ""))
	archive, err := f.Store.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "resolve-external") {
			_, _ = fmt.Fprintf(w, `{"person_id":"p","system":"msgvault","archive_id":%q,"requested_uid":"person","resolved_uid":"person","uid_state":"current"}`, archive)
			return
		}
		if r.URL.Query().Get("assertion_cursor") == "next" {
			_, _ = fmt.Fprint(w, `{"person_id":"p","state":"curated","revision":1,"assertions":[]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"person_id":"p","state":"curated","revision":1,"assertions_next_cursor":"next","assertions_truncated":true,"assertions":[{"person_id":"p","assertion_id":"a","content_version_id":%q,"role":"depicted","action":"assert"},{"person_id":"p","assertion_id":"b","content_version_id":%q,"role":"depicted","action":"assert"}]}`, version, version2)
	}))
	defer remote.Close()
	client, err := docbankmedia.NewClient(remote.URL, nil)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: f.Store, Destination: "attachments"}
	var ids []int64
	for _, offset := range []string{"", "&occurrence_offset=10"} {
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person"+offset, nil))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var result PersonPhotosResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.True(t, result.Truncated)
		require.Equal(t, "next", result.NextCursor)
		if offset == "" {
			require.Len(t, result.Photos, 2)
			require.Len(t, result.Photos[1].Occurrences, 3)
		} else {
			require.Len(t, result.Photos, 1)
		}
		if offset == "" {
			require.Equal(t, 10, *result.NextOccurrenceOffset)
			require.True(t, result.Photos[0].OccurrencesTruncated)
		} else {
			require.Nil(t, result.NextOccurrenceOffset)
			require.False(t, result.Photos[0].OccurrencesTruncated)
		}
		for _, occurrence := range result.Photos[0].Occurrences {
			ids = append(ids, occurrence.MessageID)
		}
	}
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person&cursor=next", nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var last PersonPhotosResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &last))
	require.Empty(t, last.Photos)
	require.False(t, last.Truncated)
	require.Len(t, ids, 11)
	require.Len(t, slices.Compact(slices.Sorted(slices.Values(ids))), 11)
}

func TestPersonPhotosUnsupportedRoute(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('person')`)
	require.NoError(t, err)
	remote := httptest.NewServer(http.NotFoundHandler())
	defer remote.Close()
	client, err := docbankmedia.NewClient(remote.URL, nil)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: st, Destination: "attachments"}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/people/by-uid/photos?uid=person", ""},
		{http.MethodPut, "/api/v1/people/by-uid/photo-link", `{"uid":"person","person_id":"p","revision":1,"state":"unlinked"}`},
	} {
		req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Contains(t, response.Body.String(), "Docbank person lookup unsupported")
	}
}

func TestPersonPhotosSelectedPersonMissing(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('person')`)
	require.NoError(t, err)
	archive, err := st.ArchiveUIDContext(t.Context())
	require.NoError(t, err)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "resolve-external") {
			_, _ = fmt.Fprintf(w, `{"person_id":"p","system":"msgvault","archive_id":%q,"requested_uid":"person","resolved_uid":"person","uid_state":"current"}`, archive)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found"}`))
	}))
	defer remote.Close()
	client, err := docbankmedia.NewClient(remote.URL, nil)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: st, Destination: "attachments"}
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person", nil))
	require.Equal(t, http.StatusNotFound, response.Code)
}
