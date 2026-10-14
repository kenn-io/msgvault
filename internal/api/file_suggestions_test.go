package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func suggestionFixture(t *testing.T, names []string, ready bool) (*Server, *storetest.Fixture, []int64) {
	t.Helper()
	f := storetest.New(t)
	message := f.CreateMessage("suggestion-fixture")
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	var ids []int64
	for _, name := range names {
		var id int64
		require.NoError(t, tx.QueryRow(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,mime_type,storage_path) VALUES(?,?,'application/pdf','') RETURNING id`), message, name).Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, tx.Commit())
	if ready {
		drainSuggestionProjection(t, f.Store)
	}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: f.Store, Logger: testLogger()})
	return srv, f, ids
}
func drainSuggestionProjection(t *testing.T, s *store.Store) {
	t.Helper()
	for range 100 {
		batch, err := s.FilenameDiscoveryBatchContext(t.Context())
		require.NoError(t, err)
		if batch.Status.Phase == "ready" && !batch.More {
			return
		}
	}
	require.FailNow(t, "synthetic projection did not drain")
}
func suggestionPost(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, r)
	return w
}
func suggestionBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(w.Body.Bytes()))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&body))
	return body
}

func suggestionRows(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	values, ok := body["suggestions"].([]any)
	require.True(t, ok)
	rows := make([]map[string]any, len(values))
	for i, value := range values {
		row, ok := value.(map[string]any)
		require.True(t, ok)
		rows[i] = row
	}
	return rows
}

func TestFileSuggestionsHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv, f, ids := suggestionFixture(t, []string{"reprot.pdf", "reporting.pdf", "report.pdf", "report.pdf"}, true)
	w := suggestionPost(t, srv, "/api/v1/files/suggest", `{"filename_query":"report.pdf","limit":2}`)
	require.Equal(200, w.Code, w.Body.String())
	body := suggestionBody(t, w)
	rows := suggestionRows(t, body)
	require.Len(rows, 2)
	for i, id := range []int64{ids[3], ids[2]} { // Identical names list the newest occurrence first.
		row := rows[i]
		assert.Equal(json.Number(strconv.FormatInt(id, 10)), row["id"])
		assert.Equal("exact", row["filename_match"])
		assert.Contains(row, "occurred_at")
		assert.Nil(row["occurred_at"])
		assert.NotContains(row, "storage_path")
		assert.NotContains(row, "content_hash")
	}
	assert.Equal(false, body["candidate_limit_reached"])
	assert.NotEmpty(body["index_revision"])
	for _, field := range []string{"total_count", "next_cursor", "cache_revision", "files"} {
		assert.NotContains(body, field)
	}
	// The projection is deliberately stale. Authority must remove this old name.
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET filename='unrelated.pdf' WHERE id=?`), ids[2])
	require.NoError(err)
	w = suggestionPost(t, srv, "/api/v1/files/suggest", `{"filename_query":"report.pdf"}`)
	require.Equal(200, w.Code, w.Body.String())
	rows = suggestionRows(t, suggestionBody(t, w))
	require.Len(rows, 3)
	assert.Equal(json.Number(strconv.FormatInt(ids[3], 10)), rows[0]["id"])
	assert.Equal("prefix", rows[1]["filename_match"])
	assert.Equal("typo", rows[2]["filename_match"])
}

func TestFileSuggestionsHTTPDenseCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, query, common, want, wantMatch string
		wantIndex                            int
		wantLimited                          bool
	}{
		{"all query tokens", "invoice report", "invoice other.pdf", "invoice report.pdf", "exact", 0, true},
		{"complete partial matches", "rep abc", "rep other.pdf", "rep abc.pdf", "exact", 0, false},
		{"rank before recency", "invoice rep", "invoice repzebra.pdf", "invoice rep.pdf", "exact", 0, true},
		{"prefix recency across tokens", "rep", "repzebra.pdf", "repalpha.pdf", "prefix", 600, true},
		{"typo recency across tokens", "reports", "reportz.pdf", "reporta.pdf", "typo", 600, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			names := make([]string, 601)
			for i := range names {
				names[i] = tc.common
			}
			names[tc.wantIndex] = tc.want
			srv, _, ids := suggestionFixture(t, names, true)
			w := suggestionPost(t, srv, "/api/v1/files/suggest", fmt.Sprintf(`{"filename_query":%q,"limit":1}`, tc.query))
			require.Equal(http.StatusOK, w.Code, w.Body.String())
			body := suggestionBody(t, w)
			rows := suggestionRows(t, body)
			require.Len(rows, 1)
			assert.Equal(json.Number(strconv.FormatInt(ids[tc.wantIndex], 10)), rows[0]["id"])
			assert.Equal(tc.want, rows[0]["filename"])
			assert.Equal(tc.wantMatch, rows[0]["filename_match"])
			assert.Equal(tc.wantLimited, body["candidate_limit_reached"])
		})
	}
}

func TestFileSuggestionsHTTPStrictRequest(t *testing.T) {
	_, f, _ := suggestionFixture(t, []string{"report.pdf"}, true)
	for _, body := range []string{`null`, `[]`, `{"filename_query":"report"} {}`, `{"filename_query":"!!!"}`, `{"filename_query":"report","cursor":"x"}`, `{"filename_query":"report","sort":{}}`, `{"filename_query":"report","predicate":{}}`, `{"filename_query":"report","query":"x"}`, `{"filename_query":"report","visual_query":"x"}`, `{"filename_query":"report","labels":[]}`, `{"filename_query":"report","limit":0}`, `{"filename_query":"report","limit":-1}`, `{"filename_query":"report","limit":101}`, `{"filename_query":"report","limit":1.5}`, `{"filename_query":"report","source_ids":[0]}`, `{"filename_query":"report","mime_families":["unknown"]}`, `{"filename_query":"report","after":"not-a-date"}`, `{"filename_query":"report","after":"2026-02-02T00:00:00Z","before":"2026-02-01T00:00:00Z"}`, `{"filename_query":"report","account_scopes":[{}]}`, `{"filename_query":"report","directions":["from_person"]}`, `{"filename_query":"report"` + strings.Repeat(" ", 64<<10) + `}`} {
		// The last body is a valid request padded past 64 KiB with JSON
		// whitespace, so only the body limit can reject it.
		t.Run(fmt.Sprintf("%d-%s", len(body), body[:min(40, len(body))]), func(t *testing.T) {
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: f.Store, Logger: testLogger()})
			w := suggestionPost(t, srv, "/api/v1/files/suggest", body)
			assert.Equal(t, 400, w.Code, w.Body.String())
		})
	}
}

func TestFileSuggestionsHTTPNullLimit(t *testing.T) {
	srv, f, _ := suggestionFixture(t, []string{"report.pdf"}, true)
	participantID, err := f.Store.EnsureParticipant("limit@example.test", "Limit Example", "example.test")
	require.NoError(t, err)
	person, _, err := f.Store.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	for _, path := range []string{"/api/v1/files/suggest", fmt.Sprintf("/api/v1/people/%d/files/suggest", person.ID)} {
		w := suggestionPost(t, srv, path, `{"filename_query":"report","limit":null}`)
		assert.Equal(t, 400, w.Code, w.Body.String())
	}
}
func TestFileSuggestionsHTTPReadinessDefaultsAndLimitedScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv, f, _ := suggestionFixture(t, []string{"report.pdf"}, false)
	w := suggestionPost(t, srv, "/api/v1/files/suggest", `{"filename_query":"report"}`)
	require.Equal(503, w.Code, w.Body.String())
	assert.Equal("filename_index_unavailable", suggestionBody(t, w)["error"])
	drainSuggestionProjection(t, f.Store)
	w = suggestionPost(t, srv, "/api/v1/files/suggest", `{"filename_query":"report","source_ids":[999999]}`)
	require.Equal(200, w.Code, w.Body.String())
	assert.Empty(suggestionBody(t, w)["suggestions"])
	assert.Equal(false, suggestionBody(t, w)["candidate_limit_reached"])
	names := make([]string, 601)
	for i := range names {
		names[i] = "report.pdf"
	}
	srv, _, _ = suggestionFixture(t, names, true)
	w = suggestionPost(t, srv, "/api/v1/files/suggest", `{"filename_query":"report"}`)
	require.Equal(200, w.Code, w.Body.String())
	assert.Len(suggestionBody(t, w)["suggestions"], 20)
	assert.Equal(true, suggestionBody(t, w)["candidate_limit_reached"])
	// The source filter applies during admission. All 4,207 postings fit the
	// scoped work budget, so the empty result is complete rather than limited.
	w = suggestionPost(t, srv, "/api/v1/files/suggest", `{"filename_query":"report","source_ids":[999999]}`)
	require.Equal(200, w.Code, w.Body.String())
	assert.Empty(suggestionBody(t, w)["suggestions"])
	assert.Equal(false, suggestionBody(t, w)["candidate_limit_reached"])
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/files/suggest", strings.NewReader(`{"filename_query":"report"}`)).WithContext(canceled)
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	require.Equal(http.StatusServiceUnavailable, w.Code, w.Body.String())
	assert.Equal("query_canceled", suggestionBody(t, w)["error"])
}

func TestPersonFileSuggestionsHTTP(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv, f, ids := suggestionFixture(t, []string{"report.pdf", "reprot.pdf"}, true)
	member, err := f.Store.EnsureParticipant("member@example.test", "Member Example", "example.test")
	require.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(member)
	require.NoError(err)
	var message int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT message_id FROM attachments WHERE id=?`), ids[0]).Scan(&message))
	require.NoError(f.Store.ReplaceMessageRecipients(message, "cc", []int64{member}, []string{"Member Example"}))
	path := fmt.Sprintf("/api/v1/people/%d/files/suggest", person.ID)
	w := suggestionPost(t, srv, path, `{"filename_query":"report.pdf","directions":["to_person"]}`)
	require.Equal(200, w.Code, w.Body.String())
	rows := suggestionRows(t, suggestionBody(t, w))
	require.Len(rows, 2)
	provenance, ok := rows[0]["person_provenance"].(map[string]any)
	require.True(ok)
	assert.Equal([]any{json.Number(strconv.FormatInt(member, 10))}, provenance["participant_ids"])
	assert.Equal([]any{"cc"}, provenance["roles"])
	assert.Equal([]any{"to_person"}, provenance["directions"])
	w = suggestionPost(t, srv, path, `{"filename_query":"report.pdf","directions":["from_person"]}`)
	require.Equal(200, w.Code, w.Body.String())
	assert.Empty(suggestionBody(t, w)["suggestions"])
	w = suggestionPost(t, srv, path, `{"filename_query":"report.pdf","directions":["unknown"]}`)
	assert.Equal(400, w.Code, w.Body.String())
	w = suggestionPost(t, srv, "/api/v1/people/999999/files/suggest", `{"filename_query":"report.pdf"}`)
	assert.Equal(404, w.Code, w.Body.String())
	// Oversized mandatory person binding population is an error, not a prefix.
	tx, err := f.Store.DB().BeginTx(t.Context(), nil)
	require.NoError(err)
	for i := range 64 {
		var participant int64
		require.NoError(tx.QueryRow(f.Store.Rebind(`INSERT INTO participants(email_address) VALUES(?) RETURNING id`), fmt.Sprintf("overflow%03d@example.test", i)).Scan(&participant))
		_, err = tx.Exec(f.Store.Rebind(`INSERT INTO person_participants(person_id,participant_id) VALUES(?,?)`), person.ID, participant)
		require.NoError(err)
	}
	require.NoError(tx.Commit())
	w = suggestionPost(t, srv, path, `{"filename_query":"report.pdf"}`)
	require.Equal(422, w.Code, w.Body.String())
	assert.Equal("filename_scope_too_large", suggestionBody(t, w)["error"])
	assert.NotContains(suggestionBody(t, w), "suggestions")
}

func TestFileSuggestionsHTTPOperationGate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, f, ids := suggestionFixture(t, []string{"report.pdf"}, true)
	member, err := f.Store.EnsureParticipant("gate@example.test", "Gate Example", "example.test")
	require.NoError(err)
	person, _, err := f.Store.CreatePersonFromParticipant(member)
	require.NoError(err)
	var message int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT message_id FROM attachments WHERE id=?`), ids[0]).Scan(&message))
	require.NoError(f.Store.ReplaceMessageRecipients(message, "to", []int64{member}, []string{"Gate Example"}))
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "synthetic-mutation")
	require.True(ok)
	defer release()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: f.Store, Logger: testLogger(), OperationGate: gate})
	for _, path := range []string{"/api/v1/files/suggest", fmt.Sprintf("/api/v1/people/%d/files/suggest", person.ID)} {
		w := suggestionPost(t, srv, path, `{"filename_query":"report.pdf"}`)
		require.Equal(200, w.Code, w.Body.String())
		assert.Len(suggestionBody(t, w)["suggestions"], 1)
	}
	assert.Nil(srv.analyticsState.Load().engine, "suggestions never require an analytical engine")
	assert.False(srv.analyticsState.Load().analyticsInitializationActive, "suggestions never start analytics initialization")
}

func TestFileSuggestionsHTTPRestrictedKeys(t *testing.T) {
	_, f, _ := suggestionFixture(t, []string{"report.pdf"}, true)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key", AgentAccess: true, RemoteClients: []config.RemoteClientConfig{{ClientID: "synthetic-reader", APIKey: "synthetic-reader-key"}}}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: f.Store, Logger: testLogger()})
	registry := agentgrant.NewRegistry()
	srv.agentGrants = registry
	_, secret, _, err := registry.Issue("synthetic-search", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead, agentgrant.PermissionAttachmentRead}, []agentgrant.SourceRef{{ID: f.Source.ID, Type: f.Source.SourceType, Identifier: f.Source.Identifier}}, time.Time{})
	require.NoError(t, err)
	_, valid := registry.Lookup(secret)
	require.True(t, valid)
	for _, path := range []string{"/api/v1/files/suggest", "/api/v1/people/1/files/suggest"} {
		for _, auth := range []struct {
			header, key string
			status      int
		}{{"X-Api-Key", "synthetic-reader-key", http.StatusForbidden}, {apiprotocol.AgentTokenHeader, secret, http.StatusUnauthorized}} {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"filename_query":"report"}`))
			r.Header.Set(auth.header, auth.key)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			assert.Equal(t, auth.status, w.Code, w.Body.String())
		}
	}
}

func TestFileSuggestionsHTTPContentStates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	_, f, ids := suggestionFixture(t, []string{"report.pdf", "report.pdf", "report.pdf", "report.pdf"}, true)
	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		id         int64
		path, hash string
	}{{ids[1], "https://files.example.test/report.pdf", strings.Repeat("b", 64)}, {ids[2], "", hash}, {ids[3], "aa/" + hash, ""}} {
		_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE attachments SET storage_path=?,content_hash=? WHERE id=?`), tc.path, tc.hash, tc.id)
		require.NoError(err)
	}
	dir := t.TempDir()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}, Store: f.Store, Logger: testLogger()})
	w := suggestionPost(t, srv, "/api/v1/files/suggest", `{"filename_query":"report.pdf"}`)
	require.Equal(200, w.Code, w.Body.String())
	rows := suggestionRows(t, suggestionBody(t, w))
	require.Len(rows, 4)
	states := []string{"local_content", "missing_blob", "url_only", "metadata_only"} // Newest first.
	for i, row := range rows {
		assert.Equal(states[i], row["content_state"])
		assert.Equal(i == 0, row["content_available"])
		assert.NotContains(row, "url")
		assert.NotContains(row, "storage_path")
		assert.NotContains(row, "content_hash")
	}
}

type pausedFileSuggestionBlobStore struct {
	inner  AttachmentBlobStore
	opened chan struct{}
	resume <-chan struct{}
}

func (s pausedFileSuggestionBlobStore) OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	content, size, err := s.inner.OpenStream(ctx, hash)
	if err != nil {
		return nil, 0, err
	}
	s.opened <- struct{}{}
	select {
	case <-s.resume:
		return content, size, nil
	case <-ctx.Done():
		_ = content.Close()
		return nil, 0, ctx.Err()
	}
}

func TestFileSuggestionsHTTPReleasesSnapshotBeforeBlobResolution(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, f, ids := suggestionFixture(t, []string{"report.pdf"}, true)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	content := []byte("packed attachment bytes")
	entry := buildTestPack(t, cfg.AttachmentsDir(), content)
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET content_hash=?,size=? WHERE id=?`), entry.BlobHash, len(content), ids[0])
	require.NoError(err)
	require.NoError(f.Store.RecordPackedBlobs(store.PackRecord{
		PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now(),
	}, []store.PackIndexEntry{entry}))
	blobs, err := attachmentstore.New(store.NewPackCatalog(f.Store), cfg.AttachmentsDir())
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(blobs.Close()) })
	opened := make(chan struct{}, 1)
	resume := make(chan struct{})
	var resumeOnce sync.Once
	resumeResolver := func() { resumeOnce.Do(func() { close(resume) }) }
	srv := NewServerWithOptions(ServerOptions{
		Config: cfg, Store: f.Store, Logger: testLogger(),
		BlobStore: pausedFileSuggestionBlobStore{inner: blobs, opened: opened, resume: resume},
	})
	f.Store.DB().SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/files/suggest", strings.NewReader(`{"filename_query":"report.pdf"}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	defer func() {
		resumeResolver()
		select {
		case <-handlerDone:
		case <-ctx.Done():
		}
	}()
	go func() {
		srv.Router().ServeHTTP(w, r)
		close(handlerDone)
	}()
	select {
	case <-opened:
	case <-ctx.Done():
		require.FailNow("blob resolver did not reach the physical read")
	}

	queryCtx, cancelQuery := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelQuery()
	queryDone := make(chan error, 1)
	go func() {
		var value int
		err := f.Store.DB().QueryRowContext(store.WithoutReadSnapshotContext(queryCtx), "SELECT 1").Scan(&value)
		if err == nil && value != 1 {
			err = fmt.Errorf("unexpected query value %d", value)
		}
		queryDone <- err
	}()
	select {
	case err := <-queryDone:
		require.NoError(err)
	case <-queryCtx.Done():
		require.FailNow("database query stayed blocked while blob resolution was paused")
	}
	resumeResolver()
	select {
	case <-handlerDone:
	case <-ctx.Done():
		require.FailNow("file suggestion handler did not finish after blob resolution resumed")
	}

	require.Equal(http.StatusOK, w.Code, w.Body.String())
	rows := suggestionRows(t, suggestionBody(t, w))
	require.Len(rows, 1)
	assert.Equal("local_content", rows[0]["content_state"])
	assert.Equal(true, rows[0]["content_available"])
}

func TestFileSuggestionsHTTPInputBounds(t *testing.T) {
	_, f, _ := suggestionFixture(t, []string{"report.pdf"}, true)
	for _, request := range []map[string]any{
		{"source_ids": positiveSuggestionSources(65)},
		{"mime_families": strings.Split(strings.Repeat("pdf,", 8)+"pdf", ",")},
		{"account_scopes": validSuggestionAccounts(17)},
		{"account_scopes": []map[string]any{{"addresses": strings.Split(strings.Repeat("member@example.test,", 64)+"member@example.test", ",")}}},
		{"filename_query": strings.Repeat("a", 64) + " " + strings.Repeat("b", 64)},
		{"filename_query": strings.Repeat("a ", 8) + "a"},
		{"filename_query": strings.Repeat("a", 65)},
	} {
		if _, ok := request["filename_query"]; !ok {
			request["filename_query"] = "report"
		}
		raw, err := json.Marshal(request)
		require.NoError(t, err)
		srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: f.Store, Logger: testLogger()})
		w := suggestionPost(t, srv, "/api/v1/files/suggest", string(raw))
		require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func positiveSuggestionSources(n int) []int64 {
	result := make([]int64, n)
	for i := range result {
		result[i] = 1
	}
	return result
}
func validSuggestionAccounts(n int) []map[string]any {
	result := make([]map[string]any, n)
	for i := range result {
		result[i] = map[string]any{"unattributed": true}
	}
	return result
}

func TestFileSuggestionsHTTPDateOffsets(t *testing.T) {
	srv, f, ids := suggestionFixture(t, []string{"report.pdf"}, true)
	instant := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET sent_at=? WHERE id=(SELECT message_id FROM attachments WHERE id=?)`), instant, ids[0])
	require.NoError(t, err)
	for _, tc := range []struct {
		filter string
		want   int
	}{
		{`"after":"2026-08-10T15:00:00+03:00"`, 1},
		{`"after":"2026-08-10T08:00:01-04:00"`, 0},
		{`"before":"2026-08-10T16:00:00+04:00"`, 0},
		{`"before":"2026-08-10T08:00:01-04:00"`, 1},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			w := suggestionPost(t, srv, "/api/v1/files/suggest", `{"filename_query":"report.pdf",`+tc.filter+`}`)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Len(t, suggestionBody(t, w)["suggestions"], tc.want)
		})
	}
}
