package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

type meetingDecisionTestStore struct {
	*mockStore

	err         error
	left, right store.MeetingLinkRef
	decision    string
}

func (s *meetingDecisionTestStore) DecideMeetingLinkContext(_ context.Context, left, right store.MeetingLinkRef, decision string) (store.MeetingLinkDecisionResult, error) {
	s.left, s.right, s.decision = left, right, decision
	return store.MeetingLinkDecisionResult{Left: "msg:1", Right: "msg:2", State: "link"}, s.err
}

func TestMeetingDecisionOwnerHTTP(t *testing.T) {
	backend := &meetingDecisionTestStore{mockStore: &mockStore{stats: &StoreStats{}}}
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: backend, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
	t.Cleanup(srv.agentGrants.Close)
	for _, tc := range []struct {
		name, body string
		err        error
		status     int
		want       string
	}{
		{"owner", `{"left":"msg:1","right":"msg:2","decision":"link"}`, nil, http.StatusOK, `"state":"link"`},
		{"invalid", `{"left":"msg:1","right":"msg:2","decision":"invalid"}`, store.ErrInvalidMeetingLinkDecision, http.StatusBadRequest, ""},
		{"conflict", `{"left":"msg:1","right":"msg:2","decision":"link"}`, store.ErrMeetingLinkConflict, http.StatusConflict, "meeting_link_conflict"},
		{"same copy", `{"left":"msg:1","right":"msg:2","decision":"link"}`, store.ErrMeetingLinkSameCopy, http.StatusBadRequest, store.ErrMeetingLinkSameCopy.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			backend.err = tc.err
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", "owner")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			requirements.Equal(tc.status, w.Code, w.Body.String())
			if tc.want != "" {
				assertions.Contains(w.Body.String(), tc.want)
			}
			if tc.name == "owner" {
				assertions.Equal(store.MeetingLinkRef{Kind: "msg", MessageID: 1}, backend.left)
				assertions.Equal(store.MeetingLinkRef{Kind: "msg", MessageID: 2}, backend.right)
				assertions.Equal("link", backend.decision)
			}
		})
	}
}
