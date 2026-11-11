package api

import (
	"encoding/base64"
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

func TestPersonPhotosOnlyVisibleConfirmedAttachments(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid, display_name) VALUES ('person', 'Example')`)
	require.NoError(err)
	const version = "00000000-0000-4000-8000-000000000001"
	hash := strings.Repeat("a", 64)
	for _, name := range []string{"live", "generic"} {
		mid := f.CreateMessage(name)
		if name == "live" {
			require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "nonimage", Filename: "copy.pdf", MIMEType: "application/pdf", ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))
		}
		mime := "image/jpeg"
		if name == "generic" {
			mime = "application/octet-stream"
		}
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "image", Filename: name + ".jpg", MIMEType: mime, ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))
	}
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash, MIMEType: "image/jpeg"}, store.DocbankAttachmentRef{NodeID: 7, VersionID: version}, ""))
	archive, err := f.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "resolve-external") {
			_, _ = fmt.Fprintf(w, `{"person_id":"p","system":"msgvault","archive_id":%q,"requested_uid":"person","resolved_uid":"person","uid_state":"current"}`, archive)
			return
		}
		_, _ = fmt.Fprintf(w, `{"person_id":"p","state":"curated","revision":1,"assertions":[{"person_id":"p","assertion_id":"a","content_version_id":%q,"role":"depicted","action":"assert"},{"person_id":"p","assertion_id":"b","content_version_id":%q,"role":"depicted","action":"withdrawn"},{"person_id":"p","assertion_id":"replacement","content_version_id":"00000000-0000-4000-8000-000000000002","role":"depicted","action":"assert"}]}`, version, version)
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
	require.Len(result.Photos[0].Occurrences, 3)
	var filenames []string
	for _, occurrence := range result.Photos[0].Occurrences {
		filenames = append(filenames, occurrence.Filename)
	}
	require.ElementsMatch([]string{"copy.pdf", "live.jpg", "generic.jpg"}, filenames)

}

func TestPersonPhotosOccurrencePages(t *testing.T) {
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('person')`)
	require.NoError(t, err)
	versions := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}
	hashes := []string{strings.Repeat("b", 64), strings.Repeat("c", 64)}
	for i := range 101 {
		mid := f.CreateMessage(fmt.Sprintf("message-%d", i))
		for _, part := range []string{"original", "copy"} {
			require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: part, Filename: part, ContentHash: hashes[0], StoragePath: "bb/" + hashes[0], State: "stored"}))
		}
		if i < 3 {
			require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "other", Filename: "other", ContentHash: hashes[1], StoragePath: "cc/" + hashes[1], State: "stored"}))
		}
	}
	for i, version := range versions {
		require.NoError(t, f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hashes[i]}, store.DocbankAttachmentRef{NodeID: int64(i + 1), VersionID: version}, ""))
	}
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
		_, _ = fmt.Fprintf(w, `{"person_id":"p","state":"curated","revision":1,"assertions_next_cursor":"next","assertions_truncated":true,"assertions":[{"person_id":"p","assertion_id":"a","content_version_id":%q,"role":"depicted","action":"assert"},{"person_id":"p","assertion_id":"b","content_version_id":%q,"role":"depicted","action":"assert"}]}`, versions[0], versions[1])
	}))
	defer remote.Close()
	client, err := docbankmedia.NewClient(remote.URL, nil)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: f.Store, Destination: "attachments"}
	expected := map[string]bool{}
	rows, err := f.Store.DB().QueryContext(t.Context(), `SELECT id, content_hash FROM attachments`)
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		var hash string
		require.NoError(t, rows.Scan(&id, &hash))
		version := versions[0]
		if hash == hashes[1] {
			version = versions[1]
		}
		expected[fmt.Sprintf("%s/%d", version, id)] = true
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person&occurrence_cursor="+cursor, nil))
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var result PersonPhotosResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.True(t, result.Truncated)
		require.Equal(t, "next", result.NextCursor)
		for _, photo := range result.Photos {
			for _, occurrence := range photo.Occurrences {
				key := fmt.Sprintf("%s/%d", photo.ContentVersionID, occurrence.AttachmentID)
				require.False(t, seen[key])
				seen[key] = true
			}
		}
		if page == 0 {
			require.NotEmpty(t, result.NextOccurrenceCursor)
			require.True(t, result.Photos[0].OccurrencesTruncated)
			mid := result.Photos[0].Occurrences[0].MessageID
			var otherID int64
			require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id FROM attachments WHERE message_id=? AND content_hash=?`), mid, hashes[1]).Scan(&otherID))
			delete(expected, fmt.Sprintf("%s/%d", versions[1], otherID))
			_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`), mid)
			require.NoError(t, err)
		}
		cursor = result.NextOccurrenceCursor
		if cursor == "" {
			break
		}
		require.Less(t, page, 5)
	}
	// The first message's other-version attachment disappears before its page.
	require.Equal(t, expected, seen)
	future := base64.RawURLEncoding.EncodeToString([]byte(`{"version":2,"v":"00000000-0000-4000-8000-000000000001","a":1}`))
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person&cursor=next", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var last PersonPhotosResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &last))
	require.Empty(t, last.Photos)
	for _, cursor := range []string{"invalid", future} {
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person&occurrence_cursor="+cursor, nil))
		require.Equal(t, http.StatusBadRequest, response.Code)
	}
}

func TestPersonPhotosUnsupportedRoute(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, err := st.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('person')`)
	require.NoError(t, err)
	writes := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		writes++
		w.WriteHeader(http.StatusOK)
	}))
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
		{http.MethodPut, "/api/v1/people/by-uid/photo-link", `{"uid":"person","person_id":"00000000-0000-4000-8000-000000000001","revision":1,"state":"unlinked"}`},
	} {
		req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.Contains(t, response.Body.String(), "Docbank person lookup unsupported")
	}
	require.Zero(t, writes)
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

func TestPersonPhotosInvalidAssertionCursor(t *testing.T) {
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
		cursor := r.URL.Query().Get("assertion_cursor")
		code := "validation"
		if cursor == "00000000-0000-1000-8000-000000000001" {
			code = "invalid_person"
		} else {
			assert.Equal(t, "invalid", cursor)
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprintf(w, `{"code":%q}`, code)
	}))
	defer remote.Close()
	client, err := docbankmedia.NewClient(remote.URL, nil)
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: st, Destination: "attachments"}
	for _, cursor := range []string{"invalid", "00000000-0000-1000-8000-000000000001"} {
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person&cursor="+cursor, nil))
		require.Equal(t, http.StatusUnprocessableEntity, response.Code)
	}
}

func TestPersonPhotoUnlink(t *testing.T) {
	const person = "00000000-0000-4000-8000-000000000001"
	const other = "00000000-0000-4000-8000-000000000002"
	for _, tc := range []struct {
		name, uid, linkedUID, wantUID string
		remote                        map[string]string
		status, remoteStatus          int
		mapped                        bool
	}{
		{name: "explicit retired UID", uid: "current", linkedUID: "old", wantUID: "old", status: 200},
		{name: "retired link", uid: "current", wantUID: "old", remote: map[string]string{"old": person}, status: 200},
		{name: "unlinked retired UID", uid: "old", wantUID: "old", status: 200},
		{name: "mapped UID", uid: "mapped", wantUID: "current", remote: map[string]string{"current": person}, status: 200, mapped: true},
		{name: "unlinked mapped UID", uid: "mapped", wantUID: "current", status: 200, mapped: true},
		{name: "UID belongs elsewhere", uid: "current", linkedUID: "elsewhere", status: 400},
		{name: "conflicting links", uid: "current", remote: map[string]string{"current": person, "old": other}, status: 409},
		{name: "stale revision", uid: "old", linkedUID: "old", wantUID: "old", status: 412, remoteStatus: 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			var id int64
			require.NoError(t, st.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid,display_name) VALUES ('current',?) RETURNING id`, strings.Repeat("é", 101)).Scan(&id))
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_uid_aliases (retired_uid,surviving_person_id,reason) VALUES ('old',?,'merge')`), id)
			require.NoError(t, err)
			if tc.mapped {
				_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: "test", BaseURL: "https://contacts.example.test/dav", Username: "example", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/one/", DisplayName: "Example"}}})
				require.NoError(t, err)
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_resources (address_book_id, href, remote_uid, remote_etag, remote_body, remote_semantic_hash, local_hash, mapping_status, governance, person_id) VALUES (?, ?, ?, ?, ?, ?, ?, 'mapped', 'remote', ?)`), books[0].ID, "https://contacts.example.test/books/one/person.vcf", "mapped", `"one"`, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:mapped\r\nFN:Example\r\nEND:VCARD\r\n"), "remote", "local", id)
				require.NoError(t, err)
			}
			archive, err := st.ArchiveUIDContext(t.Context())
			require.NoError(t, err)
			reads := map[string]int{}
			writes := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					uid := r.URL.Query().Get("uid")
					reads[uid]++
					if linked := tc.remote[uid]; linked != "" {
						_, _ = fmt.Fprintf(w, `{"person_id":%q,"system":"msgvault","archive_id":%q,"requested_uid":%q,"resolved_uid":%q,"uid_state":"current"}`, linked, archive, uid, uid)
					} else {
						w.WriteHeader(http.StatusNotFound)
						_, _ = w.Write([]byte(`{"code":"not_found"}`))
					}
					return
				}
				writes++
				assert.Equal(t, `"7"`, r.Header.Get("If-Match"))
				var identity docbankmedia.ExternalPersonIdentity
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&identity))
				assert.Equal(t, tc.wantUID, identity.UID)
				assert.Equal(t, "unlinked", identity.UIDState)
				assert.Equal(t, archive, identity.ArchiveID)
				assert.Empty(t, identity.DisplayNameSnapshot)
				if tc.remoteStatus != 0 {
					w.WriteHeader(tc.remoteStatus)
					return
				}
				w.Header().Set("ETag", `"8"`)
				_, _ = fmt.Fprintf(w, `{"system":"msgvault","archive_id":%q,"uid":%q,"uid_state":"unlinked","uid_kind":"vcard_uid"}`, archive, identity.UID)
			}))
			defer remote.Close()
			client, err := docbankmedia.NewClient(remote.URL, nil)
			require.NoError(t, err)
			cfg := config.NewDefaultConfig()
			cfg.Integrations.Docbank.Enabled = true
			cfg.Integrations.Docbank.AttachmentUploadConsent = true
			server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
			server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: st, Destination: "attachments"}
			payload, err := json.Marshal(PersonPhotoLinkRequest{UID: tc.uid, PersonID: person, Revision: 7, State: "unlinked", LinkedUID: tc.linkedUID})
			require.NoError(t, err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/v1/people/by-uid/photo-link", strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, req)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			if tc.status == http.StatusConflict || tc.status == http.StatusBadRequest {
				require.Zero(t, writes)
			} else {
				require.Equal(t, 1, writes)
			}
			for _, n := range reads {
				require.Equal(t, 1, n)
			}
			if tc.status == http.StatusOK {
				require.Contains(t, response.Body.String(), `"linked_uid":"`+tc.wantUID+`"`)
				require.Contains(t, response.Body.String(), `"revision":8`)
			}
		})
	}
	t.Run("invalid person ID", func(t *testing.T) {
		cfg := config.NewDefaultConfig()
		server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/v1/people/by-uid/photo-link", strings.NewReader(`{"uid":"current","person_id":"bad","revision":7,"state":"unlinked"}`))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, req)
		require.Equal(t, http.StatusUnprocessableEntity, response.Code)
	})
}
