package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
)

type fakeKataIssueOperations struct {
	selectors []kataevidence.Selector
	key       string
	created   kataissues.CreateInput
	linkedRef string
	err       error
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
		{kataissues.ErrOutsideProject, http.StatusUnprocessableEntity, "kata_issue_outside_project"},
	} {
		operations.err = tc.err
		failed := serveKataIssue(server, "/api/v1/integrations/kata/issues", createBody, map[string]string{"Idempotency-Key": "key-2"})
		assert.Equal(tc.status, failed.Code)
		assert.Contains(failed.Body.String(), tc.code)
	}
}
