package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/savedview"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/visual"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type verificationResultReadSpy struct {
	calls int
}

func (s *verificationResultReadSpy) SearchMedia(context.Context, daemonclient.MediaSearchOptions) (generated.MediaSearchResponse, error) {
	s.calls++
	return generated.MediaSearchResponse{}, nil
}

func (s *verificationResultReadSpy) SearchVisualAttachments(context.Context, VisualSearchRequest) (*visual.SearchResponse, error) {
	s.calls++
	return &visual.SearchResponse{}, nil
}

func (s *verificationResultReadSpy) SearchDocuments(context.Context, store.DocumentSearchRequest) (store.DocumentSearchResponse, error) {
	s.calls++
	return store.DocumentSearchResponse{}, nil
}

func (s *verificationResultReadSpy) SearchPersonFiles(context.Context, PersonFileSearchRequest) (generated.PersonFileSearchHTTPResponse, error) {
	s.calls++
	return generated.PersonFileSearchHTTPResponse{}, nil
}

func TestMaskedOwnerResultToolsRequireExplicitReveal(t *testing.T) {
	assertions := assert.New(t)

	spy := &verificationResultReadSpy{}
	view := store.SavedView{
		ID: 7, Name: "Synthetic view", CanonicalState: json.RawMessage(`{"query":"synthetic"}`),
		SchemaVersion: 1, Revision: 1,
	}
	views := &savedViewServiceFixture{views: []store.SavedView{view}, page: &savedview.RunPage{View: view, ResultKind: savedview.ResultEntries}}
	drafts := &draftTestRunner{result: DraftCommandResult{Stdout: `{"status":"done"}`}}
	h := newTestHandlers(&querytest.MockEngine{})
	h.mediaSearcher = spy
	h.visualSearcher = spy
	h.documentSearcher = spy
	h.personFileSearcher = spy
	h.savedViews = views
	h.drafts = drafts
	h.unmaskVerification = false

	cases := []struct {
		name    string
		args    map[string]any
		handler func(*handlers) toolHandler
	}{
		{name: ToolSearchMedia, args: map[string]any{toolArgQuery: "synthetic query"}, handler: func(h *handlers) toolHandler { return h.searchMedia }},
		{name: ToolSearchVisualAttachments, args: map[string]any{bodyFormatText: "synthetic query"}, handler: func(h *handlers) toolHandler { return h.searchVisualAttachments }},
		{name: ToolSearchDocuments, args: map[string]any{toolArgQuery: "synthetic query"}, handler: func(h *handlers) toolHandler { return h.searchDocuments }},
		{name: ToolSearchPersonFiles, args: map[string]any{toolArgPersonID: float64(7)}, handler: func(h *handlers) toolHandler { return h.searchPersonFiles }},
		{name: ToolRunSavedView, args: map[string]any{"id": float64(7)}, handler: func(h *handlers) toolHandler { return h.runSavedView }},
		{name: ToolDraftGet, args: map[string]any{"draft_id": "synthetic-draft"}, handler: func(h *handlers) toolHandler {
			return func(ctx context.Context, req toolRequest) (*toolResult, error) {
				return draftToolHandler("draft-get", "draft_id")(h, ctx, req)
			}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := callToolDirect(t, tc.name, tc.handler(h), tc.args)
			require.True(t, result.isError, result.text)
			assert.Contains(t, resultText(t, result), "verification_masked")
		})
	}
	assertions.Zero(spy.calls)
	assertions.Zero(views.runID)
	assertions.Zero(drafts.calls)

	h.unmaskVerification = true
	for _, tc := range cases {
		t.Run(tc.name+"/unmask", func(t *testing.T) {
			result := callToolDirect(t, tc.name, tc.handler(h), tc.args)
			require.False(t, result.isError, result.text)
		})
	}
	assertions.Equal(4, spy.calls)
	assertions.Equal(int64(7), views.runID)
	assertions.Equal(1, drafts.calls)
}

func TestMaskedOwnerDraftResponsesRequireExplicitReveal(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	drafts := &draftTestRunner{
		result: DraftCommandResult{Stdout: `{"candidate_content":"Your verification code is 482913","raw_mime":"From: sender@example.test"}`},
		err:    &DraftCommandError{Message: "existing draft subject contains 482913", Stderr: `{"candidate_content":"Your verification code is 482913","raw_mime":"From: sender@example.test"}`},
	}
	h := newTestHandlers(&querytest.MockEngine{})
	h.drafts = drafts
	h.unmaskVerification = false

	cases := []struct {
		name, command, positional string
		args                      map[string]any
	}{
		{name: ToolDraftReply, command: "draft-reply", positional: "message_id", args: map[string]any{"message_id": float64(7), "body": "reply"}},
		{name: ToolDraftCompose, command: "draft-compose", args: map[string]any{"subject": "subject", "body": "body"}},
		{name: ToolDraftForward, command: "draft-forward", positional: "message_id", args: map[string]any{"message_id": float64(7)}},
		{name: ToolDraftGet, command: "draft-get", positional: "draft_id", args: map[string]any{"draft_id": "synthetic-draft"}},
		{name: ToolDraftEdit, command: "draft-edit", positional: "draft_id", args: map[string]any{"draft_id": "synthetic-draft", "revision": float64(1), "body": "replacement"}},
		{name: ToolDraftDelete, command: "draft-delete", positional: "draft_id", args: map[string]any{"draft_id": "synthetic-draft", "revision": float64(1)}},
		{name: ToolDraftRecover, command: "draft-recover", positional: "draft_id", args: map[string]any{"draft_id": "synthetic-draft", "revision": float64(1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := func(ctx context.Context, req toolRequest) (*toolResult, error) {
				return draftToolHandler(tc.command, tc.positional)(h, ctx, req)
			}
			result := callToolDirect(t, tc.name, handler, tc.args)
			requirements.True(result.isError, result.text)
			assertions.Contains(result.text, "verification_masked")
			assertions.NotContains(result.text, "482913")
			assertions.NotContains(result.text, "candidate_content")
			assertions.NotContains(result.text, "raw_mime")
		})
	}
	assertions.Zero(drafts.calls)
}
