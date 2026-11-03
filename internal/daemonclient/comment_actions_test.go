package daemonclient_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meta"
)

func TestDaemonClientCommentActionNoRetry(t *testing.T) {
	for _, scenario := range []string{"disconnect", "invalid JSON", "partial error"} {
		t.Run(scenario, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assertions.Equal("POST", r.Method)
				assertions.Equal("/api/v1/messages/7/comment-actions", r.URL.Path)
				if scenario == "disconnect" {
					hijacker, ok := w.(http.Hijacker)
					if !assertions.True(ok) {
						return
					}
					conn, _, err := hijacker.Hijack()
					if assertions.NoError(err) {
						_ = conn.Close()
					}
					return
				}
				if scenario == "partial error" {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = fmt.Fprint(w, `{"code":"remote_accepted_local_failed","message":"Repair local evidence","result":{"receipt":{"id":3,"state":"succeeded"}}}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"broken"`)
			}))
			defer srv.Close()
			c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
			requirements.NoError(err)
			defer func() { _ = c.Close() }()
			result, err := c.RunCommentAction(t.Context(), 7, meta.ActionRequest{Action: meta.ActionHide})
			var failure *meta.ActionError
			requirements.ErrorAs(err, &failure)
			if scenario == "partial error" {
				assertions.Equal("remote_accepted_local_failed", failure.Code)
				requirements.NotNil(result)
				assertions.Equal(int64(3), result.Receipt.ID)
			} else {
				assertions.Equal("outcome_unknown", failure.Code)
			}
			assertions.Equal(int32(1), calls.Load())
		})
	}
}
func TestDaemonClientCommentReadsAndTriage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/comment-queue":
			assertions.Equal("handled", r.URL.Query().Get("state"))
			assertions.Equal("1", r.URL.Query().Get("limit"))
			assertions.Equal("7", r.URL.Query().Get("before_id"))
			assertions.Equal("app:9:page:100", r.URL.Query().Get("source"))
			_, _ = fmt.Fprint(w, `{"items":[{"message_id":8}],"next_before_id":8}`)
		case "/api/v1/messages/7/comment-actions":
			if r.Method == http.MethodPost {
				var body meta.ActionRequest
				if !assertions.NoError(json.UnmarshalRead(r.Body, &body)) {
					return
				}
				assertions.Equal(meta.ActionDelete, body.Action)
				assertions.Equal("binding", body.Confirm)
				assertions.Equal("sent", body.ResolveUnknown)
				_, _ = fmt.Fprint(w, `{"message_id":7,"receipt":{"id":3,"state":"succeeded"}}`)
			} else {
				_, _ = fmt.Fprint(w, `[{"id":3,"state":"succeeded"}]`)
			}
		case "/api/v1/messages/7/comment-triage":
			assertions.Equal("PUT", r.Method)
			var body api.CommentTriageBody
			if !assertions.NoError(json.UnmarshalRead(r.Body, &body)) {
				return
			}
			assertions.Equal("open", body.Expect)
			assertions.Equal("handled", body.State)
			_, _ = fmt.Fprint(w, `{"state":"handled"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	requirements.NoError(err)
	defer func() { _ = c.Close() }()
	result, err := c.RunCommentAction(t.Context(), 7, meta.ActionRequest{Action: meta.ActionDelete, Confirm: "binding", ResolveUnknown: "sent"})
	requirements.NoError(err)
	assertions.Equal(int64(7), result.MessageID)
	rows, err := c.ListCommentActions(t.Context(), 7)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Equal(int64(3), rows[0].ID)
	state, err := c.UpdateCommentTriage(t.Context(), 7, "handled", "open")
	requirements.NoError(err)
	assertions.Equal("handled", state)
	page, err := c.ListCommentQueue(t.Context(), api.CommentQueueRequest{Source: "app:9:page:100", State: "handled", Limit: 1, BeforeID: 7})
	requirements.NoError(err)
	assertions.Equal(int64(8), page.NextBeforeID)
}

func TestDaemonClientCommentActionStandardRefusal(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{401, "unauthorized"}, {503, "operation_in_progress"}} {
		t.Run(tc.code, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":%q,"message":"Request refused before dispatch"}`, tc.code)
			}))
			defer srv.Close()
			c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
			requirements.NoError(err)
			defer func() { _ = c.Close() }()
			_, err = c.RunCommentAction(t.Context(), 7, meta.ActionRequest{Action: meta.ActionHide})
			if tc.code == "operation_in_progress" {
				var refusal *daemonclient.OperationInProgressError
				requirements.ErrorAs(err, &refusal)
			} else {
				var refusal *daemonclient.APIError
				requirements.ErrorAs(err, &refusal)
				assertions.Equal(tc.code, refusal.Code)
				assertions.Equal(tc.status, refusal.Status)
			}
			assertions.Equal(1, calls)
		})
	}
}
