package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type fakeKataIssueOperations struct {
	selectors []kataevidence.Selector
	key       string
	created   kataissues.CreateInput
	linkedRef string
	found     [2]int64
	read      string
	offset    int
	err       error
}

func (f *fakeKataIssueOperations) Find(_ context.Context, messageID, attachmentID int64) ([]taskclient.KataTask, bool, error) {
	f.found = [2]int64{messageID, attachmentID}
	return []taskclient.KataTask{{UID: "01ISSUE", Ref: "abcd", QualifiedRef: "example#abcd", Project: "example", Title: "Send the budget", Status: "closed", Revision: "3"}}, true, f.err
}

func (f *fakeKataIssueOperations) Context(_ context.Context, ref string, offset int) (kataissues.Context, error) {
	f.read, f.offset = ref, offset
	return kataissues.Context{Issue: taskclient.KataTask{UID: "01ISSUE", QualifiedRef: "example#abcd"}, Passages: []kataissues.ContextPassage{{State: kataevidence.Changed, SavedQuote: "Send the budget"}}, NextOffset: 20}, f.err
}

func (f *fakeKataIssueOperations) Prepare(_ context.Context, selectors []kataevidence.Selector) ([]kataevidence.Evidence, error) {
	f.selectors = selectors
	return []kataevidence.Evidence{{ID: "evidence-1", Excerpt: "Send the budget", NextRune: 1000, ContentTrust: "untrusted"}}, f.err
}

func (f *fakeKataIssueOperations) Create(_ context.Context, key string, input kataissues.CreateInput) (kataissues.Result, error) {
	f.key, f.created = key, input
	return kataissues.Result{Issue: taskclient.KataTask{UID: "01ISSUE", Ref: "abcd", QualifiedRef: "example#abcd", Project: "example", Title: input.Title, Status: "open", Revision: "1", WebURL: "javascript:alert(1)"}}, f.err
}

func (f *fakeKataIssueOperations) Link(_ context.Context, ref string, _ []kataevidence.Reference) (taskclient.KataTask, error) {
	f.linkedRef = ref
	return taskclient.KataTask{UID: "01ISSUE", Ref: "abcd", QualifiedRef: "example#abcd", Project: "example", Status: "open", Revision: "2"}, f.err
}

func serveKataIssue(server *Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	server.router.ServeHTTP(recorder, request)
	return recorder
}

func TestKataIssueHTTP(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	operations := &fakeKataIssueOperations{}
	server := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(), KataIssueOperations: operations})

	prepared := serveKataIssue(server, kataEvidencePreparePath, `{"selectors":[{"kind":"message","message_id":7,"start_rune":1000,"max_chars":1000}]}`, nil)
	require.Equal(http.StatusOK, prepared.Code, prepared.Body.String())
	var preparedBody KataEvidencePrepareResponse
	require.NoError(json.Unmarshal(prepared.Body.Bytes(), &preparedBody))
	assert.Equal(1000, preparedBody.Evidence[0].NextRune)
	require.Len(operations.selectors, 1)
	assert.Equal(1000, *operations.selectors[0].StartRune)

	// The largest quotes a client may send still fit the request bound.
	selectors := make([]string, kataevidence.MaxReferences)
	for i := range selectors {
		selectors[i] = `{"kind":"message","message_id":` + strconv.Itoa(i+1) + `,"quote":"` + strings.Repeat("🙂", kataevidence.MaxChars) + `"}`
	}
	largestBody := `{"selectors":[` + strings.Join(selectors, ",") + `]}`
	require.Greater(len(largestBody), 64<<10)
	largest := serveKataIssue(server, kataEvidencePreparePath, largestBody, nil)
	require.Equal(http.StatusOK, largest.Code, largest.Body.String())
	assert.Len(operations.selectors, kataevidence.MaxReferences)

	createBody := `{"title":"Send the budget","list":"follow up","person_id":3,"evidence":[{"version":1,"kind":"message","archive_uid":"archive","message_id":7,"source_type":"email","source_identifier":"inbox@example.com","source_message_id":"m7","message":{"body_sha256":"` + strings.Repeat("ab", 32) + `","start_rune":0,"end_rune":5}}]}`
	missingKey := serveKataIssue(server, "/api/v1/integrations/kata/issues", createBody, nil)
	assert.Equal(http.StatusPreconditionRequired, missingKey.Code)
	assert.Empty(operations.key, "a key the daemon rejects never reaches the service")

	created := serveKataIssue(server, "/api/v1/integrations/kata/issues", createBody, map[string]string{"Idempotency-Key": "key-1"})
	require.Equal(http.StatusCreated, created.Code, created.Body.String())
	var createdBody KataIssueResponse
	require.NoError(json.Unmarshal(created.Body.Bytes(), &createdBody))
	assert.Equal("example#abcd", createdBody.Issue.QualifiedRef)
	assert.Empty(createdBody.Issue.WebURL, "only http(s) issue links reach the browser")
	assert.Equal("key-1", operations.key)
	assert.Equal("follow up", operations.created.List)
	require.NotNil(operations.created.PersonID)
	assert.Equal(int64(3), *operations.created.PersonID)

	linked := serveKataIssue(server, "/api/v1/integrations/kata/issues/example%23abcd/evidence", `{"evidence":[]}`, nil)
	require.Equal(http.StatusOK, linked.Code, linked.Body.String())
	assert.Equal("example#abcd", operations.linkedRef)

	lookup := httptest.NewRecorder()
	server.router.ServeHTTP(lookup, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/kata/issues?message_id=7&attachment_id=3", nil))
	require.Equal(http.StatusOK, lookup.Code, lookup.Body.String())
	var found KataIssueListResponse
	require.NoError(json.Unmarshal(lookup.Body.Bytes(), &found))
	assert.Equal([2]int64{7, 3}, operations.found)
	require.Len(found.Issues, 1)
	assert.Equal("closed", found.Issues[0].Status)
	assert.True(found.Truncated)
	missing := httptest.NewRecorder()
	server.router.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/kata/issues?attachment_id=3", nil))
	assert.Equal(http.StatusBadRequest, missing.Code)

	read := httptest.NewRecorder()
	server.router.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/kata/issues/example%23abcd/context?offset=10", nil))
	require.Equal(http.StatusOK, read.Code, read.Body.String())
	assert.Equal("no-store", read.Header().Get("Cache-Control"))
	var issueContext KataIssueContextResponse
	require.NoError(json.Unmarshal(read.Body.Bytes(), &issueContext))
	assert.Equal("example#abcd", operations.read)
	assert.Equal(10, operations.offset)
	require.Len(issueContext.Passages, 1)
	assert.Equal("Send the budget", issueContext.Passages[0].SavedQuote)
	assert.Equal(20, issueContext.NextOffset)
	negative := httptest.NewRecorder()
	server.router.ServeHTTP(negative, httptest.NewRequest(http.MethodGet, "/api/v1/integrations/kata/issues/example%23abcd/context?offset=-1", nil))
	assert.Equal(http.StatusBadRequest, negative.Code)

	kataServer := httptest.NewServer(katatest.New(t).Service.Handler())
	t.Cleanup(kataServer.Close)
	cfg := &config.Config{}
	cfg.Integrations.Kata = config.TaskIntegrationConfig{Enabled: true, Endpoint: kataServer.URL, APIKey: katatest.Token, DefaultProject: "example"}
	f := storetest.New(t)
	backendServer := NewServerWithOptions(ServerOptions{Config: cfg, Store: &mockStore{}, Logger: testLogger(), KataIssueOperations: &kataIssueBackend{store: f.Store, config: cfg.Integrations.Kata}})
	var createRequest KataIssueCreateRequest
	require.NoError(json.Unmarshal([]byte(createBody), &createRequest))
	linkBody, err := json.Marshal(KataEvidenceLinkRequest{Evidence: createRequest.Evidence})
	require.NoError(err)
	for _, ref := range []string{"example#", "example#a#b", "#abcd", "example#abcd/efgh", `example#abcd\efgh`, "example#abcd?efgh", "example#.", "example#.."} {
		for _, route := range []string{"context", "evidence"} {
			path := "/api/v1/integrations/kata/issues/" + url.PathEscape(ref) + "/" + route
			malformed := httptest.NewRecorder()
			if route == "context" {
				backendServer.router.ServeHTTP(malformed, httptest.NewRequest(http.MethodGet, path, nil))
			} else {
				malformed = serveKataIssue(backendServer, path, string(linkBody), nil)
			}
			assert.Equal(http.StatusBadRequest, malformed.Code, "%s %s: %s", route, ref, malformed.Body.String())
			var body ErrorResponse
			require.NoError(json.Unmarshal(malformed.Body.Bytes(), &body))
			assert.Equal("invalid_ref", body.Error)
			assert.Equal("Kata issue ref is malformed", body.Message)
		}
	}

	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{kataevidence.ErrChanged, http.StatusConflict, "evidence_changed"},
		{kataevidence.ErrInvalidReference, http.StatusBadRequest, "invalid_evidence"},
		{kataissues.ErrInvalidRequest, http.StatusUnprocessableEntity, "invalid_request"},
		{fmt.Errorf("%w: person 404: %w", personagenda.ErrIdentityLookup, fmt.Errorf("get person: %w", store.ErrPersonNotFound)), http.StatusNotFound, "person_profile_not_found"},
		{kataevidence.ErrUnprocessed, http.StatusUnprocessableEntity, "evidence_unprocessed"},
		{kataevidence.ErrUnsupported, http.StatusUnprocessableEntity, "evidence_unsupported"},
	} {
		operations.err = tc.err
		failed := serveKataIssue(server, "/api/v1/integrations/kata/issues", createBody, map[string]string{"Idempotency-Key": "key-2"})
		assert.Equal(tc.status, failed.Code)
		assert.Contains(failed.Body.String(), tc.code)
	}
}

func TestKataIssueLookupArchiveErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*testing.T, *storetest.Fixture) int64
		status int
		code   string
	}{
		{"archive identity missing", func(t *testing.T, f *storetest.Fixture) int64 {
			t.Helper()
			_, err := f.Store.DB().Exec("DELETE FROM archive_metadata WHERE key='archive_uid'")
			require.NoError(t, err)
			return 1
		}, http.StatusServiceUnavailable, "archive_unavailable"},
		{"database closed", func(t *testing.T, f *storetest.Fixture) int64 {
			t.Helper()
			require.NoError(t, f.Store.DB().Close())
			return 1
		}, http.StatusServiceUnavailable, "archive_unavailable"},
		{"message missing", func(_ *testing.T, _ *storetest.Fixture) int64 {
			return 1
		}, http.StatusNotFound, "evidence_unavailable"},
		{"source message ID missing", func(t *testing.T, f *storetest.Fixture) int64 {
			t.Helper()
			id := f.CreateMessage("lookup-message")
			_, err := f.Store.DB().Exec(f.Store.Rebind("UPDATE messages SET source_message_id='' WHERE id=?"), id)
			require.NoError(t, err)
			return id
		}, http.StatusUnprocessableEntity, "evidence_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := storetest.New(t)
			id := tc.setup(t, f)
			server := NewServerWithOptions(ServerOptions{
				Config: &config.Config{}, Store: &mockStore{}, Logger: testLogger(),
				KataIssueOperations: &kataIssueBackend{store: f.Store},
			})
			response := httptest.NewRecorder()
			server.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/integrations/kata/issues?message_id=%d", id), nil))
			assert.Equal(t, tc.status, response.Code)
			var body struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			assert.Equal(t, tc.code, body.Error)
		})
	}
}
