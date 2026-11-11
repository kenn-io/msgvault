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

func TestPersonPhotosInvalidCursor(t *testing.T) {
	cfg := config.NewDefaultConfig()
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=example&cursor=2.eyJjIjoieCJ9", nil))
	require.Equal(t, http.StatusBadRequest, response.Code)
}

func TestPersonPhotosUnlinkedAndAmbiguousUID(t *testing.T) {
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var id int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid, display_name) VALUES ('canonical', 'Example') RETURNING id`).Scan(&id))
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_uid_aliases (retired_uid, surviving_person_id, reason) VALUES ('retired', ?, 'merge')`), id)
	require.NoError(err)
	_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: "test", BaseURL: "https://contacts.example.test/dav", Username: "example", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/one/", DisplayName: "Example"}}})
	require.NoError(err)
	archive, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, archive, r.URL.Query().Get("archive_id"))
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"not_found"}`))
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
	response := get("canonical")
	require.Equal(http.StatusOK, response.Code)
	require.Contains(response.Body.String(), `"link_state":"unlinked"`)
	var otherID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('other') RETURNING id`).Scan(&otherID))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_resources (address_book_id, href, remote_uid, remote_etag, remote_body, remote_semantic_hash, local_hash, mapping_status, governance, person_id) VALUES (?, ?, ?, ?, ?, ?, ?, 'mapped', 'remote', ?)`), books[0].ID, "https://contacts.example.test/books/one/other.vcf", "retired", `"two"`, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:retired\r\nFN:Other\r\nEND:VCARD\r\n"), "remote", "local", otherID)
	require.NoError(err)
	require.Equal(http.StatusConflict, get("canonical").Code)
}

func TestPersonPhotosOnlyConfirmedAttachments(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid, display_name) VALUES ('person', 'Example')`)
	require.NoError(err)
	const version = "00000000-0000-4000-8000-000000000001"
	const withdrawn = "00000000-0000-4000-8000-000000000003"
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
	withdrawnHash := strings.Repeat("d", 64)
	mid := f.CreateMessage("withdrawn")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: "withdrawn.jpg", ContentHash: withdrawnHash, StoragePath: "dd/" + withdrawnHash, State: "stored"}))
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: withdrawnHash}, store.DocbankAttachmentRef{NodeID: 8, VersionID: withdrawn}, ""))
	authorHash := strings.Repeat("e", 64)
	author := f.CreateMessage("author")
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), author, store.AttachmentWrite{Filename: "author.jpg", ContentHash: authorHash, StoragePath: "ee/" + authorHash, State: "stored"}))
	require.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: authorHash}, store.DocbankAttachmentRef{NodeID: 9, VersionID: "00000000-0000-4000-8000-000000000004"}, ""))
	archive, err := f.Store.ArchiveUIDContext(t.Context())
	require.NoError(err)
	lastReached := false
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "resolve-external") {
			_, _ = fmt.Fprintf(w, `{"person_id":"00000000-0000-4000-8000-000000000200","system":"msgvault","archive_id":%q,"requested_uid":"person","resolved_uid":"person","uid_state":"current"}`, archive)
			return
		}
		assert.Equal(t, "1", r.URL.Query().Get("assertion_limit"))
		switch r.URL.Query().Get("assertion_cursor") {
		case "":
			_, _ = fmt.Fprintf(w, `{"person_id":"00000000-0000-4000-8000-000000000200","state":"curated","revision":1,"assertions_next_cursor":"00000000-0000-4000-8000-000000000101","assertions_truncated":true,"assertions":[{"person_id":"00000000-0000-4000-8000-000000000200","assertion_id":"00000000-0000-4000-8000-000000000101","content_version_id":%q,"role":"depicted","action":"assert"}]}`, version)
		case "00000000-0000-4000-8000-000000000101":
			_, _ = fmt.Fprintf(w, `{"person_id":"00000000-0000-4000-8000-000000000200","state":"curated","revision":1,"assertions_next_cursor":"00000000-0000-4000-8000-000000000102","assertions_truncated":true,"assertions":[{"person_id":"00000000-0000-4000-8000-000000000200","assertion_id":"00000000-0000-4000-8000-000000000102","content_version_id":%q,"role":"depicted","action":"withdrawn"}]}`, withdrawn)
		case "00000000-0000-4000-8000-000000000102":
			_, _ = fmt.Fprint(w, `{"person_id":"00000000-0000-4000-8000-000000000200","state":"curated","revision":1,"assertions_next_cursor":"00000000-0000-4000-8000-000000000103","assertions_truncated":true,"assertions":[{"person_id":"00000000-0000-4000-8000-000000000200","assertion_id":"00000000-0000-4000-8000-000000000103","content_version_id":"00000000-0000-4000-8000-000000000002","role":"depicted","action":"assert"}]}`)
		default:
			lastReached = true
			_, _ = fmt.Fprint(w, `{"person_id":"00000000-0000-4000-8000-000000000200","state":"curated","revision":1,"assertions":[{"person_id":"00000000-0000-4000-8000-000000000200","assertion_id":"00000000-0000-4000-8000-000000000104","content_version_id":"00000000-0000-4000-8000-000000000004","role":"author","action":"assert"}]}`)
		}
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
	require.Equal("00000000-0000-4000-8000-000000000101", result.Photos[0].AssertionID)
	require.Len(result.Photos[0].Occurrences, 3)
	var filenames []string
	for _, occurrence := range result.Photos[0].Occurrences {
		filenames = append(filenames, occurrence.Filename)
	}
	require.ElementsMatch([]string{"copy.pdf", "live.jpg", "generic.jpg"}, filenames)
	for result.NextCursor != "" {
		response = httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person&cursor="+result.NextCursor, nil))
		require.Equal(http.StatusOK, response.Code)
		result = PersonPhotosResponse{}
		require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
		require.Empty(result.Photos)
	}
	require.True(lastReached)
}

func TestPersonPhotosAssertionDisappearsDuringPaging(t *testing.T) {
	for _, mode := range []string{"hidden", "withdrawn"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			f := storetest.New(t)
			_, err := f.Store.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('person')`)
			requirements.NoError(err)
			versions := []string{"00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000001"}
			for _, i := range []int{1, 0} {
				version := versions[i]
				hash := strings.Repeat(string(rune('a'+i)), 64)
				count := 1
				if i == 0 {
					count = 101
				}
				for n := range count {
					mid := f.CreateMessage(fmt.Sprintf("photo-%d-%d", i, n))
					requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{Filename: "photo", ContentHash: hash, StoragePath: "aa/" + hash, State: "stored"}))
				}
				requirements.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{NodeID: int64(i + 1), VersionID: version}, ""))
			}
			archive, err := f.Store.ArchiveUIDContext(t.Context())
			requirements.NoError(err)
			changed := false
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "resolve-external") {
					_, _ = fmt.Fprintf(w, `{"person_id":"00000000-0000-4000-8000-000000000200","system":"msgvault","archive_id":%q,"requested_uid":"person","resolved_uid":"person","uid_state":"current"}`, archive)
					return
				}
				assertions := []docbankmedia.PersonAssertion{}
				if !changed || mode == "withdrawn" {
					action := "assert"
					if changed {
						action = "withdrawn"
					}
					assertions = append(assertions, docbankmedia.PersonAssertion{PersonID: "00000000-0000-4000-8000-000000000200", AssertionID: "00000000-0000-4000-8000-000000000001", ContentVersionID: versions[0], Role: "depicted", Action: action})
				}
				assertions = append(assertions, docbankmedia.PersonAssertion{PersonID: "00000000-0000-4000-8000-000000000200", AssertionID: "00000000-0000-4000-8000-000000000101", ContentVersionID: versions[1], Role: "depicted", Action: "assert"})
				cursor := r.URL.Query().Get("assertion_cursor")
				page := []docbankmedia.PersonAssertion{}
				for _, a := range assertions {
					if a.AssertionID > cursor {
						page = append(page, a)
					}
				}
				next := ""
				if len(page) > 1 {
					page = page[:1]
					next = page[len(page)-1].AssertionID
				}
				assert.NoError(t, json.NewEncoder(w).Encode(docbankmedia.PersonAssertions{PersonID: "00000000-0000-4000-8000-000000000200", State: "curated", Revision: 1, Assertions: page, NextCursor: next, Truncated: next != ""}))
			}))
			defer remote.Close()
			client, err := docbankmedia.NewClient(remote.URL, nil)
			requirements.NoError(err)
			cfg := config.NewDefaultConfig()
			cfg.Integrations.Docbank.Enabled = true
			cfg.Integrations.Docbank.AttachmentUploadConsent = true
			server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
			server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: f.Store, Destination: "attachments"}
			cursor := ""
			found := false
			for n := range 4 {
				response := httptest.NewRecorder()
				server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person&cursor="+cursor, nil))
				requirements.Equal(http.StatusOK, response.Code, response.Body.String())
				var result PersonPhotosResponse
				requirements.NoError(json.Unmarshal(response.Body.Bytes(), &result))
				if n == 0 {
					requirements.Len(result.Photos, 1)
					requirements.Len(result.Photos[0].Occurrences, 100)
					changed = true
				}
				for _, photo := range result.Photos {
					if n > 0 && photo.ContentVersionID == versions[0] {
						requirements.Fail("withdrawn or hidden photo returned again")
					}
					if photo.ContentVersionID == versions[1] {
						found = true
						requirements.Len(photo.Occurrences, 1)
					}
				}
				cursor = result.NextCursor
				if cursor == "" {
					break
				}
			}
			requirements.True(found, "the later assertion must survive the earlier assertion disappearing")
		})
	}
}

func TestPersonPhotosOccurrencePages(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	_, err := f.Store.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('person')`)
	requirements.NoError(err)
	versions := []string{"00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"}
	hashes := []string{strings.Repeat("b", 64), strings.Repeat("c", 64)}
	for i := range 101 {
		mid := f.CreateMessage(fmt.Sprintf("message-%d", i))
		for _, part := range []string{"original", "copy"} {
			requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: part, Filename: part, ContentHash: hashes[0], StoragePath: "bb/" + hashes[0], State: "stored"}))
		}
		if i < 3 {
			requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), mid, store.AttachmentWrite{SourcePartKey: "other", Filename: "other", ContentHash: hashes[1], StoragePath: "cc/" + hashes[1], State: "stored"}))
		}
	}
	for i, version := range versions {
		requirements.NoError(f.Store.RecordDocbankAttachment(t.Context(), "attachments", store.DocbankAttachmentDelivery{ContentHash: hashes[i]}, store.DocbankAttachmentRef{NodeID: int64(i + 1), VersionID: version}, ""))
	}
	archive, err := f.Store.ArchiveUIDContext(t.Context())
	requirements.NoError(err)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "resolve-external") {
			_, _ = fmt.Fprintf(w, `{"person_id":"00000000-0000-4000-8000-000000000200","system":"msgvault","archive_id":%q,"requested_uid":"person","resolved_uid":"person","uid_state":"current"}`, archive)
			return
		}
		cursor := r.URL.Query().Get("assertion_cursor")
		if cursor == "" {
			_, _ = fmt.Fprint(w, `{"person_id":"00000000-0000-4000-8000-000000000200","state":"curated","revision":1,"assertions_next_cursor":"00000000-0000-4000-8000-000000000100","assertions_truncated":true,"assertions":[{"person_id":"00000000-0000-4000-8000-000000000200","assertion_id":"00000000-0000-4000-8000-000000000100","content_version_id":"10000000-0000-4000-8000-000000000001","role":"author","action":"assert"}]}`)
			return
		}
		if cursor == "00000000-0000-4000-8000-000000000101" {
			_, _ = fmt.Fprintf(w, `{"person_id":"00000000-0000-4000-8000-000000000200","state":"curated","revision":1,"assertions":[{"person_id":"00000000-0000-4000-8000-000000000200","assertion_id":"00000000-0000-4000-8000-000000000102","content_version_id":%q,"role":"depicted","action":"assert"}]}`, versions[1])
			return
		}
		_, _ = fmt.Fprintf(w, `{"person_id":"00000000-0000-4000-8000-000000000200","state":"curated","revision":1,"assertions_next_cursor":"00000000-0000-4000-8000-000000000101","assertions_truncated":true,"assertions":[{"person_id":"00000000-0000-4000-8000-000000000200","assertion_id":"00000000-0000-4000-8000-000000000101","content_version_id":%q,"role":"depicted","action":"assert"}]}`, versions[0])
	}))
	defer remote.Close()
	client, err := docbankmedia.NewClient(remote.URL, nil)
	requirements.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: f.Store, Destination: "attachments"}
	counts := map[string]int{}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; ; page++ {
		response := httptest.NewRecorder()
		server.Router().ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/people/by-uid/photos?uid=person&cursor="+cursor, nil))
		requirements.Equal(http.StatusOK, response.Code, response.Body.String())
		var result PersonPhotosResponse
		requirements.NoError(json.Unmarshal(response.Body.Bytes(), &result))
		for _, photo := range result.Photos {
			for _, occurrence := range photo.Occurrences {
				key := fmt.Sprintf("%s/%d", photo.ContentVersionID, occurrence.AttachmentID)
				requirements.False(seen[key])
				seen[key] = true
				counts[photo.ContentVersionID]++
			}
		}
		if page == 1 {
			requirements.NotEmpty(result.NextCursor)
			requirements.True(result.Photos[0].OccurrencesTruncated)
		}
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
		requirements.Less(page, 5)
	}
	requirements.Equal(202, counts[versions[0]])
	requirements.Equal(3, counts[versions[1]])
}

func TestPersonPhotosRemoteErrors(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
		resolve, status          int
		code                     string
	}{
		{name: "unsupported get", method: http.MethodGet, path: "/api/v1/people/by-uid/photos?uid=person", status: 503},
		{name: "unsupported put", method: http.MethodPut, path: "/api/v1/people/by-uid/photo-link", body: `{"uid":"person","person_id":"00000000-0000-4000-8000-000000000001","revision":1,"state":"unlinked"}`, status: 503},
		{name: "missing person", method: http.MethodGet, path: "/api/v1/people/by-uid/photos?uid=person", resolve: 200, status: 404, code: "not_found"},
		{name: "invalid assertion cursor", method: http.MethodGet, path: "/api/v1/people/by-uid/photos?uid=person&cursor=1." + base64.RawURLEncoding.EncodeToString([]byte(`{"c":"00000000-0000-1000-8000-000000000001"}`)), resolve: 200, status: 422, code: "invalid_person"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			_, err := st.DB().ExecContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('person')`)
			require.NoError(err)
			archive, err := st.ArchiveUIDContext(t.Context())
			require.NoError(err)
			writes := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					writes++
				}
				if strings.HasSuffix(r.URL.Path, "resolve-external") && tc.resolve == 200 {
					_, _ = fmt.Fprintf(w, `{"person_id":"p","system":"msgvault","archive_id":%q,"requested_uid":"person","resolved_uid":"person","uid_state":"current"}`, archive)
					return
				}
				if tc.resolve == 0 {
					http.NotFound(w, r)
					return
				}
				if tc.status == 422 {
					assert.Equal(t, "00000000-0000-1000-8000-000000000001", r.URL.Query().Get("assertion_cursor"))
				}
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"code":%q}`, tc.code)
			}))
			defer remote.Close()
			client, err := docbankmedia.NewClient(remote.URL, nil)
			require.NoError(err)
			cfg := config.NewDefaultConfig()
			cfg.Integrations.Docbank.Enabled = true
			cfg.Integrations.Docbank.AttachmentUploadConsent = true
			server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
			server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: st, Destination: "attachments"}
			req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, req)
			require.Equal(tc.status, response.Code, response.Body.String())
			if tc.resolve == 0 {
				require.Contains(response.Body.String(), "Docbank person lookup unsupported")
			}
			if tc.method == http.MethodPut {
				require.Zero(writes)
			}
		})
	}
}

func TestPersonPhotoUnlink(t *testing.T) {
	const person = "00000000-0000-4000-8000-000000000001"
	const other = "00000000-0000-4000-8000-000000000002"
	for _, tc := range []struct {
		name, uid, wantUID   string
		remote               map[string]string
		status, remoteStatus int
		mapped, forwarded    bool
		noWrites             bool
	}{
		{name: "retired link", uid: "current", wantUID: "old", remote: map[string]string{"old": person}, status: 200},
		{name: "unlinked retired UID", uid: "old", status: 200, noWrites: true},
		{name: "mapped UID", uid: "mapped", wantUID: "current", remote: map[string]string{"current": person}, status: 200, mapped: true},
		{name: "forwarded UID", uid: "current", wantUID: "old", remote: map[string]string{"current": person, "old": person}, status: 200, forwarded: true},
		{name: "conflicting links", uid: "current", remote: map[string]string{"current": person, "old": other}, status: 409},
		{name: "stale revision", uid: "old", wantUID: "old", remote: map[string]string{"old": person}, status: 412, remoteStatus: 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			var id int64
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons (vcard_uid) VALUES ('current') RETURNING id`).Scan(&id))
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_uid_aliases (retired_uid,surviving_person_id,reason) VALUES ('old',?,'merge')`), id)
			requirements.NoError(err)
			if tc.mapped {
				_, books, err := st.ReplaceCardDAVDiscoveryContext(t.Context(), store.CardDAVDiscoveryInput{ConnectionName: "test", BaseURL: "https://contacts.example.test/dav", Username: "example", PrincipalURL: "https://contacts.example.test/principal/", HomeURL: "https://contacts.example.test/books/", Books: []store.CardDAVDiscoveredBook{{CanonicalURL: "https://contacts.example.test/books/one/", DisplayName: "Example"}}})
				requirements.NoError(err)
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO carddav_resources (address_book_id, href, remote_uid, remote_etag, remote_body, remote_semantic_hash, local_hash, mapping_status, governance, person_id) VALUES (?, ?, ?, ?, ?, ?, ?, 'mapped', 'remote', ?)`), books[0].ID, "https://contacts.example.test/books/one/person.vcf", "mapped", `"one"`, []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nUID:mapped\r\nFN:Example\r\nEND:VCARD\r\n"), "remote", "local", id)
				requirements.NoError(err)
			}

			archive, err := st.ArchiveUIDContext(t.Context())
			requirements.NoError(err)
			writes := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					uid := r.URL.Query().Get("uid")
					if linked := tc.remote[uid]; linked != "" {
						resolved := uid
						if tc.forwarded && uid == "current" {
							resolved = "old"
						}
						assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"person_id": linked, "system": "msgvault", "archive_id": archive, "requested_uid": uid, "resolved_uid": resolved, "uid_state": "current", "forwarded": resolved != uid}))
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
				if tc.remoteStatus != 0 {
					w.WriteHeader(tc.remoteStatus)
					return
				}
				w.Header().Set("ETag", `"8"`)
				_, _ = fmt.Fprintf(w, `{"system":"msgvault","archive_id":%q,"uid":%q,"uid_state":"unlinked","uid_kind":"vcard_uid"}`, archive, identity.UID)
			}))
			defer remote.Close()
			client, err := docbankmedia.NewClient(remote.URL, nil)
			requirements.NoError(err)
			cfg := config.NewDefaultConfig()
			cfg.Integrations.Docbank.Enabled = true
			cfg.Integrations.Docbank.AttachmentUploadConsent = true
			server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
			server.docbankAttachments = &DocbankAttachmentReader{Client: client, Store: st, Destination: "attachments"}
			payload, err := json.Marshal(PersonPhotoLinkRequest{UID: tc.uid, PersonID: person, Revision: 7, State: "unlinked"})
			requirements.NoError(err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/v1/people/by-uid/photo-link", strings.NewReader(string(payload)))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			server.Router().ServeHTTP(response, req)
			requirements.Equal(tc.status, response.Code, response.Body.String())
			if tc.noWrites || tc.status == http.StatusConflict {
				requirements.Zero(writes)
			} else {
				requirements.Equal(1, writes)
			}
			if tc.noWrites {
				requirements.Contains(response.Body.String(), `"revision":7`)
			} else if tc.status == http.StatusOK {
				requirements.Contains(response.Body.String(), `"revision":8`)
			}
		})
	}
}

func TestPersonPhotoLinkInvalidPersonID(t *testing.T) {
	cfg := config.NewDefaultConfig()
	server := NewServerWithOptions(ServerOptions{Config: cfg, Logger: slog.New(slog.DiscardHandler)})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/api/v1/people/by-uid/photo-link", strings.NewReader(`{"uid":"current","person_id":"bad","revision":7,"state":"unlinked"}`))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.Router().ServeHTTP(response, req)
	require.Equal(t, http.StatusUnprocessableEntity, response.Code)
}
