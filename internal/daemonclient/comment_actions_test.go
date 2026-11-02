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
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "POST", r.Method)
				assert.Equal(t, "/api/v1/messages/7/comment-actions", r.URL.Path)
				if scenario == "disconnect" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if assert.NoError(t, err) {
						_ = conn.Close()
					}
					return
				}
				if scenario == "partial error" {
					w.WriteHeader(500)
					_, _ = fmt.Fprint(w, `{"code":"remote_accepted_local_failed","message":"Repair local evidence","result":{"receipt":{"id":3,"state":"succeeded"}}}`)
					return
				}
				_, _ = fmt.Fprint(w, `{"broken"`)
			}))
			defer srv.Close()
			c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			result, err := c.RunCommentAction(t.Context(), 7, meta.ActionRequest{Action: meta.ActionHide})
			var failure *meta.ActionError
			require.ErrorAs(t, err, &failure)
			if scenario == "partial error" {
				assert.Equal(t, "remote_accepted_local_failed", failure.Code)
				require.NotNil(t, result)
				assert.Equal(t, int64(3), result.Receipt.ID)
			} else {
				assert.Equal(t, "outcome_unknown", failure.Code)
			}
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}
func TestDaemonClientCommentReadsAndTriage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/comment-queue":
			assert.Equal(t, "handled", r.URL.Query().Get("state"))
			assert.Equal(t, "1", r.URL.Query().Get("limit"))
			assert.Equal(t, "7", r.URL.Query().Get("before_id"))
			assert.Equal(t, "app:9:page:100", r.URL.Query().Get("source"))
			_, _ = fmt.Fprint(w, `{"items":[{"message_id":8}],"next_before_id":8}`)
		case "/api/v1/messages/7/comment-actions":
			if r.Method == http.MethodPost {
				var body meta.ActionRequest
				require.NoError(t, json.UnmarshalRead(r.Body, &body))
				assert.Equal(t, meta.ActionDelete, body.Action)
				assert.Equal(t, "binding", body.Confirm)
				assert.Equal(t, "sent", body.ResolveUnknown)
				_, _ = fmt.Fprint(w, `{"message_id":7,"receipt":{"id":3,"state":"succeeded"}}`)
			} else {
				_, _ = fmt.Fprint(w, `[{"id":3,"state":"succeeded"}]`)
			}
		case "/api/v1/messages/7/comment-triage":
			assert.Equal(t, "PUT", r.Method)
			var body api.CommentTriageBody
			require.NoError(t, json.UnmarshalRead(r.Body, &body))
			assert.Equal(t, "open", body.Expect)
			assert.Equal(t, "handled", body.State)
			_, _ = fmt.Fprint(w, `{"state":"handled"}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c, err := daemonclient.New(daemonclient.Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	result, err := c.RunCommentAction(t.Context(), 7, meta.ActionRequest{Action: meta.ActionDelete, Confirm: "binding", ResolveUnknown: "sent"})
	require.NoError(t, err)
	assert.Equal(t, int64(7), result.MessageID)
	rows, err := c.ListCommentActions(t.Context(), 7)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(3), rows[0].ID)
	state, err := c.UpdateCommentTriage(t.Context(), 7, "handled", "open")
	require.NoError(t, err)
	assert.Equal(t, "handled", state)
	page, err := c.ListCommentQueue(t.Context(), api.CommentQueueRequest{Source: "app:9:page:100", State: "handled", Limit: 1, BeforeID: 7})
	require.NoError(t, err)
	assert.Equal(t, int64(8), page.NextBeforeID)
}
