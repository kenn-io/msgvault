package client_test

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type emailFamilyHTTPDoer struct {
	client *http.Client
}

func (d emailFamilyHTTPDoer) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	// #nosec G704 -- the request targets the httptest server created by this test.
	return d.client.Do(req)
}

func TestGeneratedDecisionPreviewAllowsOmittedActorRefAndFamilyRevision(t *testing.T) {
	input := generated.EmailFamilyDecisionInput{
		ActorKind:  "cli",
		Kind:       "handled",
		MessageIds: []int64{42},
		RequestKey: "0123456789abcdef0123456789abcdef",
	}

	require.NoError(t, input.Validate())
}

func TestGeneratedEmailFamilyClientPreservesEmptySourceSelections(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	type requestCapture struct {
		query string
		body  string
		err   error
	}
	captures := make(chan requestCapture, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture := requestCapture{}
		if r.Method == http.MethodGet {
			capture.query = r.URL.RawQuery
		} else {
			encoded, err := io.ReadAll(r.Body)
			capture.err = err
			capture.body = string(encoded)
		}
		captures <- capture
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/messages/family/decisions" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(server.Close)

	apiClient, err := runtime.NewAPIClient(server.URL, runtime.WithHTTPClient(emailFamilyHTTPDoer{client: server.Client()}))
	require.NoError(err)
	client := generated.NewClient(apiClient)
	getFamilyQuery := func(sourceIDs []int64) string {
		_, callErr := client.GetEmailFamily(t.Context(), &generated.GetEmailFamilyRequestOptions{
			PathParams: &generated.GetEmailFamilyPath{ID: 42},
			Query:      &generated.GetEmailFamilyQuery{SourceID: sourceIDs},
		})
		require.NoError(callErr)
		request := <-captures
		require.NoError(request.err)
		return request.query
	}
	assert.Equal("source_id=", getFamilyQuery([]int64{}))
	assert.Empty(getFamilyQuery(nil))

	getDecisionQuery := func(sourceIDs []int64) string {
		_, callErr := client.GetEmailFamilyDecision(t.Context(), &generated.GetEmailFamilyDecisionRequestOptions{
			PathParams: &generated.GetEmailFamilyDecisionPath{ID: 9},
			Query:      &generated.GetEmailFamilyDecisionQuery{SourceID: sourceIDs},
		})
		require.NoError(callErr)
		request := <-captures
		require.NoError(request.err)
		return request.query
	}
	assert.Equal("source_id=", getDecisionQuery([]int64{}))
	assert.Empty(getDecisionQuery(nil))

	messageID := int64(42)
	listDecisionQuery := func(sourceIDs []int64) string {
		_, callErr := client.ListEmailFamilyDecisions(t.Context(), &generated.ListEmailFamilyDecisionsRequestOptions{
			Query: &generated.ListEmailFamilyDecisionsQuery{MessageID: &messageID, SourceID: sourceIDs},
		})
		require.NoError(callErr)
		request := <-captures
		require.NoError(request.err)
		return request.query
	}
	assert.Equal("message_id=42&source_id=", listDecisionQuery([]int64{}))
	assert.Equal("message_id=42", listDecisionQuery(nil))

	input := &generated.EmailFamilyDecisionInput{
		ActorKind:  "owner",
		Kind:       "handled",
		MessageIds: []int64{42},
		RequestKey: strings.Repeat("a", 32),
		SourceIds:  []int64{},
	}
	_, err = client.ApplyEmailFamilyDecision(t.Context(), &generated.ApplyEmailFamilyDecisionRequestOptions{Body: input})
	require.NoError(err)
	postRequest := <-captures
	require.NoError(postRequest.err)
	var decoded map[string]any
	require.NoError(json.Unmarshal([]byte(postRequest.body), &decoded))
	assert.Equal([]any{}, decoded["source_ids"])

	input.SourceIds = nil
	_, err = client.ApplyEmailFamilyDecision(t.Context(), &generated.ApplyEmailFamilyDecisionRequestOptions{Body: input})
	require.NoError(err)
	postRequest = <-captures
	require.NoError(postRequest.err)
	var omitted map[string]any
	require.NoError(json.Unmarshal([]byte(postRequest.body), &omitted))
	assert.NotContains(omitted, "source_ids")

	undo := &generated.UndoEmailFamilyDecisionBody{
		ExpectedFamilyRevision: "revision",
		ExpectedRevision:       1,
		SourceIds:              []int64{},
	}
	_, err = client.UndoEmailFamilyDecision(t.Context(), &generated.UndoEmailFamilyDecisionRequestOptions{
		PathParams: &generated.UndoEmailFamilyDecisionPath{ID: 9},
		Body:       undo,
	})
	require.NoError(err)
	postRequest = <-captures
	require.NoError(postRequest.err)
	var undoDecoded map[string]any
	require.NoError(json.Unmarshal([]byte(postRequest.body), &undoDecoded))
	assert.Equal([]any{}, undoDecoded["source_ids"])

	undo.SourceIds = nil
	_, err = client.UndoEmailFamilyDecision(t.Context(), &generated.UndoEmailFamilyDecisionRequestOptions{
		PathParams: &generated.UndoEmailFamilyDecisionPath{ID: 9},
		Body:       undo,
	})
	require.NoError(err)
	postRequest = <-captures
	require.NoError(postRequest.err)
	undoOmitted := map[string]any{}
	require.NoError(json.Unmarshal([]byte(postRequest.body), &undoOmitted))
	assert.NotContains(undoOmitted, "source_ids")
}
