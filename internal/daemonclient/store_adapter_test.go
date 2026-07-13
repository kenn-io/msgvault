package daemonclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/contentverify"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestNew_RejectsHTTPWithoutAllowInsecure(t *testing.T) {
	_, err := New(Config{
		URL:    "http://nas:8080",
		APIKey: "key",
	})
	require.Error(t, err, "New() should reject http:// without AllowInsecure")
}

func TestNew_AllowsHTTPWithAllowInsecure(t *testing.T) {
	s, err := New(Config{
		URL:           "http://nas:8080",
		APIKey:        "key",
		AllowInsecure: true,
	})
	require.NoError(t, err, "New()")
	require.NotNil(t, s, "New() returned nil store")
}

func TestNew_AllowsHTTPS(t *testing.T) {
	s, err := New(Config{
		URL:    "https://nas:8080",
		APIKey: "key",
	})
	require.NoError(t, err, "New()")
	require.NotNil(t, s, "New() returned nil store")
}

func TestNew_RejectsEmptyURL(t *testing.T) {
	_, err := New(Config{APIKey: "key"})
	require.Error(t, err, "New() should reject empty URL")
}

func TestNew_RejectsInvalidScheme(t *testing.T) {
	_, err := New(Config{
		URL:    "ftp://nas:8080",
		APIKey: "key",
	})
	require.Error(t, err, "New() should reject ftp:// scheme")
	assert.ErrorContains(t, err, "http or https")
}

func TestNew_RejectsEmptyHost(t *testing.T) {
	_, err := New(Config{
		URL:           "http://",
		APIKey:        "key",
		AllowInsecure: true,
	})
	require.Error(t, err, "New() should reject URL with empty host")
	assert.ErrorContains(t, err, "must include a host")
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	s, err := New(Config{
		URL:           "http://nas:8080/",
		APIKey:        "key",
		AllowInsecure: true,
	})
	require.NoError(t, err, "New()")
	assert.Equal(t, "http://nas:8080", s.BaseURL(), "baseURL should have trailing slash trimmed")
}

func TestNew_DefaultTimeout(t *testing.T) {
	s, err := New(Config{
		URL:    "https://nas:8080",
		APIKey: "key",
	})
	require.NoError(t, err, "New()")
	assert.NotZero(t, s.Timeout(), "httpClient.Timeout should have a default")
}

func TestRunCLISyncStreamsWithoutAbsoluteClientTimeout(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/sync-full", r.URL.Path, "path")

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"begin\n"}` + "\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(srv.Close)

	st, err := New(Config{
		URL:           srv.URL,
		AllowInsecure: true,
		Timeout:       10 * time.Millisecond,
		HTTPClient:    srv.Client(),
	})
	require.NoError(err, "New")

	var output strings.Builder
	err = st.RunCLISync(context.Background(), CLISyncRequest{Full: true}, func(stream, data string) error {
		assert.Equal("stdout", stream, "stream")
		_, _ = output.WriteString(data)
		return nil
	})
	require.NoError(err, "streaming CLI sync should not use http.Client.Timeout as an absolute body-read timeout")
	assert.Equal("begin\n", output.String(), "streamed output")
}

func TestRunCLICommandStreamsOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/run", r.URL.Path, "path")
		var body struct {
			Args []string          `json:"args"`
			Env  map[string]string `json:"env"`
			Cwd  string            `json:"cwd"`
		}
		decodeErr := json.NewDecoder(r.Body).Decode(&body)
		if !assert.NoError(decodeErr, "decode request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assert.Equal([]string{"remove-account", "alice@example.com", "--yes"}, body.Args, "args")
		assert.Equal(map[string]string{"MSGVAULT_IMAP_PASSWORD": "secret"}, body.Env, "env")
		assert.Equal("/caller", body.Cwd, "cwd")

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"removed\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"warning\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(srv.Close)

	st, err := New(Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err, "New")

	var stdout strings.Builder
	var stderr strings.Builder
	err = st.RunCLICommand(
		context.Background(),
		CLIRunRequest{
			Args: []string{"remove-account", "alice@example.com", "--yes"},
			Env:  map[string]string{"MSGVAULT_IMAP_PASSWORD": "secret"},
			Cwd:  "/caller",
		},
		func(stream, data string) error {
			switch stream {
			case "stdout":
				_, _ = stdout.WriteString(data)
			case "stderr":
				_, _ = stderr.WriteString(data)
			}
			return nil
		},
	)

	require.NoError(err, "RunCLICommand")
	assert.Equal("removed\n", stdout.String(), "stdout")
	assert.Equal("warning\n", stderr.String(), "stderr")
}

func TestPlanCLIAddCalendarUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/add-calendar/plan", r.URL.Path, "path")
		var body struct {
			Email            string `json:"email"`
			OAuthApp         string `json:"oauth_app"`
			OAuthAppExplicit bool   `json:"oauth_app_explicit"`
			Headless         bool   `json:"headless"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body), "decode request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assert.Equal("alice@example.com", body.Email, "email")
		assert.Equal("acme", body.OAuthApp, "oauth app")
		assert.True(body.OAuthAppExplicit, "oauth app explicit")

		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(json.NewEncoder(w).Encode(map[string]any{
			"needs_scope_escalation": true,
			"headline":               "CALENDAR ACCESS REQUIRED",
			"body_lines":             []string{"Calendar sync needs read-only Calendar access."},
			"cancel_hint":            "Cancelled. Calendar was not added.",
			"oauth_app":              "acme",
			"oauth_app_resolved":     true,
			"needs_client_check":     true,
		}), "encode response") {
			return
		}
	}))
	t.Cleanup(srv.Close)

	st, err := New(Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err, "New")

	got, err := st.PlanCLIAddCalendar(context.Background(), CLIAddCalendarPlanRequest{
		Email:            "alice@example.com",
		OAuthApp:         "acme",
		OAuthAppExplicit: true,
	})

	require.NoError(err, "PlanCLIAddCalendar")
	require.NotNil(got, "plan")
	assert.True(got.NeedsScopeEscalation, "needs scope escalation")
	assert.Equal("CALENDAR ACCESS REQUIRED", got.Headline, "headline")
	assert.Equal([]string{"Calendar sync needs read-only Calendar access."}, got.BodyLines, "body lines")
	assert.Equal("Cancelled. Calendar was not added.", got.CancelHint, "cancel hint")
	assert.Equal("acme", got.OAuthApp, "oauth app")
	assert.True(got.OAuthAppResolved, "oauth app resolved")
	assert.True(got.NeedsClientCheck, "needs client check")
}

func TestPlanCLIEmbeddingsUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/embeddings/plan", r.URL.Path, "path")
		var body struct {
			Operation    string `json:"operation"`
			GenerationID int64  `json:"generation_id"`
			Force        bool   `json:"force"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body), "decode request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assert.Equal("activate", body.Operation, "operation")
		assert.Equal(int64(2), body.GenerationID, "generation id")
		assert.True(body.Force, "force")

		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(json.NewEncoder(w).Encode(map[string]any{
			"needs_confirmation": true,
			"prompt":             "Activate generation 2 (fp)? ",
		}), "encode response") {
			return
		}
	}))
	t.Cleanup(srv.Close)

	st, err := New(Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err, "New")

	got, err := st.PlanCLIEmbeddings(context.Background(), CLIEmbeddingsPlanRequest{
		Operation:    "activate",
		GenerationID: 2,
		Force:        true,
	})

	require.NoError(err, "PlanCLIEmbeddings")
	require.NotNil(got, "plan")
	assert.True(got.NeedsConfirmation, "needs confirmation")
	assert.Equal("Activate generation 2 (fp)? ", got.Prompt, "prompt")
}

func TestPlanCLIDeleteStagedUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/delete-staged/plan", r.URL.Path, "path")
		var body struct {
			BatchID             string `json:"batch_id"`
			Permanent           bool   `json:"permanent"`
			Yes                 bool   `json:"yes"`
			Account             string `json:"account"`
			RemoteDeleteEnabled bool   `json:"remote_delete_enabled"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body), "decode request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assert.Equal("batch-123", body.BatchID, "batch id")
		assert.True(body.Permanent, "permanent")
		assert.False(body.Yes, "yes")
		assert.Equal("alice@example.com", body.Account, "account")
		assert.True(body.RemoteDeleteEnabled, "remote delete enabled")

		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(json.NewEncoder(w).Encode(map[string]any{
			"stdout":                       "Deletion Summary:\n",
			"needs_execution":              true,
			"needs_confirmation":           true,
			"confirmation_mode":            "permanent",
			"planned_batch_ids":            []string{"batch-123"},
			"plan_fingerprint":             "fp-client",
			"needs_scope_escalation":       true,
			"scope_escalation_headline":    "PERMISSION UPGRADE REQUIRED",
			"scope_escalation_body_lines":  []string{"Batch deletion requires elevated Gmail permissions."},
			"scope_escalation_cancel_hint": "Cancelled.",
			"scope_escalation_account":     "alice@example.com",
			"scope_escalation_oauth_app":   "acme",
			"remote_delete_env_var":        "MSGVAULT_ENABLE_REMOTE_DELETE",
		}), "encode response") {
			return
		}
	}))
	t.Cleanup(srv.Close)

	st, err := New(Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err, "New")

	got, err := st.PlanCLIDeleteStaged(context.Background(), CLIDeleteStagedPlanRequest{
		BatchID:             "batch-123",
		Permanent:           true,
		Account:             "alice@example.com",
		RemoteDeleteEnabled: true,
	})

	require.NoError(err, "PlanCLIDeleteStaged")
	require.NotNil(got, "plan")
	assert.Equal("Deletion Summary:\n", got.Stdout, "stdout")
	assert.True(got.NeedsExecution, "needs execution")
	assert.True(got.NeedsConfirmation, "needs confirmation")
	assert.Equal("permanent", got.ConfirmationMode, "confirmation mode")
	assert.Equal([]string{"batch-123"}, got.PlannedBatchIDs, "planned batch ids")
	assert.Equal("fp-client", got.PlanFingerprint, "plan fingerprint")
	assert.True(got.NeedsScopeEscalation, "needs scope escalation")
	assert.Equal("PERMISSION UPGRADE REQUIRED", got.ScopeEscalationHeadline, "scope headline")
	assert.Equal([]string{"Batch deletion requires elevated Gmail permissions."}, got.ScopeEscalationBodyLines, "scope body")
	assert.Equal("Cancelled.", got.ScopeEscalationCancelHint, "scope cancel hint")
	assert.Equal("alice@example.com", got.ScopeEscalationAccount, "scope account")
	assert.Equal("acme", got.ScopeEscalationOAuthApp, "scope oauth app")
	assert.Equal("MSGVAULT_ENABLE_REMOTE_DELETE", got.RemoteDeleteEnvVar, "remote delete env")
}

func TestCreateCLIDeletionManifestUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	manifest := deletion.NewManifest("tui selection", []string{"gid1", "gid2"})
	manifest.CreatedBy = "tui"

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/deletion-manifests", r.URL.Path, "path")
		var body deletion.Manifest
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body), "decode request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assert.Equal(manifest.ID, body.ID, "manifest id")
		assert.Equal("tui", body.CreatedBy, "created by")
		assert.Equal([]string{"gid1", "gid2"}, body.GmailIDs, "gmail ids")

		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(json.NewEncoder(w).Encode(map[string]any{
			"id":            manifest.ID,
			"message_count": 2,
		}), "encode response") {
			return
		}
	})

	got, err := s.CreateCLIDeletionManifest(context.Background(), manifest)
	require.NoError(err, "CreateCLIDeletionManifest")
	require.NotNil(got, "result")
	assert.Equal(manifest.ID, got.ID, "id")
	assert.Equal(2, got.MessageCount, "message count")
}

func TestPlanCLIDeduplicateUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/deduplicate/plan", r.URL.Path, "path")
		var body struct {
			Account                    string `json:"account"`
			Collection                 string `json:"collection"`
			Prefer                     string `json:"prefer"`
			ContentHash                bool   `json:"content_hash"`
			DeleteDupsFromSourceServer bool   `json:"delete_dups_from_source_server"`
		}
		if !assert.NoError(json.NewDecoder(r.Body).Decode(&body), "decode request") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		assert.Equal("alice@example.com", body.Account, "account")
		assert.Empty(body.Collection, "collection")
		assert.Equal("gmail,mbox", body.Prefer, "prefer")
		assert.True(body.ContentHash, "content hash")
		assert.True(body.DeleteDupsFromSourceServer, "delete from source")

		w.Header().Set("Content-Type", "application/json")
		if !assert.NoError(json.NewEncoder(w).Encode(map[string]any{
			"prefix_stdout": "Deduping across collection\n",
			"items": []map[string]any{
				{
					"source_id":           42,
					"scope_label":         "alice@example.com",
					"scope_is_collection": false,
					"stdout":              "Duplicate groups found: 1\n",
					"duplicate_messages":  2,
					"backfilled_count":    3,
					"plan_fingerprint":    "fp-client",
					"needs_confirmation":  true,
				},
			},
			"footer_stdout": "No duplicates found in any source.\n",
		}), "encode response") {
			return
		}
	}))
	t.Cleanup(srv.Close)

	st, err := New(Config{URL: srv.URL, AllowInsecure: true, HTTPClient: srv.Client()})
	require.NoError(err, "New")

	got, err := st.PlanCLIDeduplicate(context.Background(), CLIDeduplicatePlanRequest{
		Account:                    "alice@example.com",
		Prefer:                     "gmail,mbox",
		ContentHash:                true,
		DeleteDupsFromSourceServer: true,
	})

	require.NoError(err, "PlanCLIDeduplicate")
	require.NotNil(got, "plan")
	assert.Equal("Deduping across collection\n", got.PrefixStdout, "prefix")
	assert.Equal("No duplicates found in any source.\n", got.FooterStdout, "footer")
	require.Len(got.Items, 1, "items")
	assert.Equal(int64(42), got.Items[0].SourceID, "source id")
	assert.Equal("alice@example.com", got.Items[0].ScopeLabel, "scope label")
	assert.Equal("Duplicate groups found: 1\n", got.Items[0].Stdout, "stdout")
	assert.Equal(2, got.Items[0].DuplicateMessages, "duplicate messages")
	assert.Equal(int64(3), got.Items[0].BackfilledCount, "backfilled count")
	assert.Equal("fp-client", got.Items[0].PlanFingerprint, "fingerprint")
	assert.True(got.Items[0].NeedsConfirmation, "needs confirmation")
}

// newTestStore creates a Client pointing at the given httptest server.
func newTestStore(srv *httptest.Server, apiKey string) *Client {
	st, err := New(Config{
		URL:           srv.URL,
		APIKey:        apiKey,
		AllowInsecure: true,
		HTTPClient:    srv.Client(),
	})
	if err != nil {
		panic(err)
	}
	return st
}

func newCLINDJSONTestServer(t *testing.T, method, path string, events ...string) *httptest.Server {
	t.Helper()

	assert := assert.New(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(method, r.Method, "method")
		assert.Equal(path, r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, event := range events {
			_, _ = w.Write([]byte(event + "\n"))
		}
	}))
}

func stringPtr(v string) *string {
	out := new(string)
	*out = v
	return out
}

func int64Ptr(v int64) *int64 {
	out := new(int64)
	*out = v
	return out
}

func generatedMessageSummaryFixture(subject string) generated.MessageSummary {
	return generated.MessageSummary{
		ID:              42,
		ConversationID:  int64Ptr(7),
		Subject:         subject,
		MessageType:     stringPtr("sms"),
		From:            "Alice <alice@example.com>",
		FromEmail:       stringPtr("alice@example.com"),
		FromName:        stringPtr("Alice"),
		FromPhone:       stringPtr("+15555550123"),
		To:              []string{"bob@example.com"},
		Cc:              []string{"carol@example.com"},
		Bcc:             []string{"dave@example.com"},
		SentAt:          "2024-01-15T10:30:00Z",
		DeletedAt:       stringPtr("2026-03-18T15:00:00Z"),
		Snippet:         "preview",
		Labels:          []string{"INBOX"},
		HasAttachments:  true,
		SizeBytes:       1234,
		SourceMessageID: stringPtr("msg-42"),
	}
}

func newGeneratedClientAdapterStore(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	generatedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "generated-key", r.Header.Get("X-Api-Key"), "api key")
		handler(w, r)
	}))
	t.Cleanup(generatedSrv.Close)

	return newTestStore(generatedSrv, "generated-key")
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(value), "encode JSON response")
}

func TestGeneratedClientUsesStoreTransportAndAuth(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/query", r.URL.Path, "path")
		assert.Equal("secret-key", r.Header.Get("X-Api-Key"), "api key")
		assert.Equal("application/json", r.Header.Get("Accept"), "accept")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":["n"],"rows":[[1]],"row_count":1}`))
	}))
	t.Cleanup(srv.Close)

	s := newTestStore(srv, "secret-key")
	c, err := s.GeneratedClient()
	require.NoError(err, "generated client")

	got, err := c.RunQuery(context.Background(), &generated.RunQueryRequestOptions{
		Body: &generated.RunQueryBody{SQL: "SELECT 1"},
	})

	require.NoError(err, "RunQuery")
	assert.Equal([]string{"n"}, got.Columns, "columns")
	require.Len(got.Rows, 1, "rows")
	assert.InDelta(float64(1), got.Rows[0][0], 0, "scalar cell")
}

func TestGeneratedCLIResponseErrorRejectsMissingJSON200Payload(t *testing.T) {
	err := CLIResponseError(&generated.CreateCLICollectionResp{
		StatusCode: http.StatusOK,
	}, nil)

	require.Error(t, err, "missing CLI JSON200 payload must not be a successful zero-value mutation")
	assert.ErrorContains(t, err, "200 JSON response body")
}

func TestGetStatsRejectsEmptyGeneratedOKBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/cli/stats", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	_, err := s.GetCLIStats(context.Background(), "", "")

	require.Error(t, err, "empty 200 JSON body must fail")
	assert.NotContains(t, err.Error(), "API error (200)", "empty success decode failures are not API errors")
}

func TestCreateCLICollectionRejectsEmptyGeneratedOKBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "method")
		assert.Equal(t, "/api/v1/cli/collections", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	_, err := s.CreateCLICollection(context.Background(), CLICollectionCreateRequest{Name: "archive"})

	require.Error(t, err, "empty 200 mutation body must fail")
	assert.NotContains(t, err.Error(), "API error (200)", "empty success decode failures are not API errors")
}

func TestHandleErrorResponse_JSONBody(t *testing.T) {
	body := `{"error":"not_found","message":"Message 42 not found"}`
	resp := &http.Response{
		StatusCode: http.StatusNotFound,
		Body:       http.NoBody,
	}
	// Use a real body
	resp.Body = readCloser(body)

	err := HandleErrorResponse(resp)
	require.Error(t, err, "HandleErrorResponse should return error")
	require.ErrorContains(t, err, "404", "error should contain status code")
	assert.ErrorContains(t, err, "Message 42 not found", "error should contain API message")
}

func TestHandleErrorResponse_PlainTextBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       readCloser("internal server error"),
	}

	err := HandleErrorResponse(resp)
	require.Error(t, err, "HandleErrorResponse should return error")
	require.ErrorContains(t, err, "500", "error should contain status code")
	assert.ErrorContains(t, err, "internal server error", "error should contain body text")
}

func TestGeneratedAPIResponseErrorDecodesGeneratedErrorResponse(t *testing.T) {
	resp := &generated.GetStatsResp{
		StatusCode: http.StatusInternalServerError,
		Body:       []byte(`{"error":"db_error","message":"database locked"}`),
	}

	err := APIResponseError(resp, nil)

	require.Error(t, err, "generated API response error")
	assert.ErrorContains(t, err, "API error (500): database locked")
}

func TestGeneratedAPIResponseErrorUsesResponseWhenGeneratedClientReturnsError(t *testing.T) {
	resp := &generated.GetStatsResp{
		StatusCode: http.StatusServiceUnavailable,
		Body:       []byte(`{"error":"daemon_unavailable","message":"daemon unavailable"}`),
	}

	err := APIResponseError(resp, errors.New("API error (status 503)"))

	require.Error(t, err, "generated API response error")
	assert.ErrorContains(t, err, "API error (503): daemon unavailable")
}

func TestGeneratedAPIResponseErrorReturnsTransportErrorWithoutResponse(t *testing.T) {
	transportErr := errors.New("connection refused")

	err := APIResponseError(nil, transportErr)

	require.ErrorIs(t, err, transportErr, "transport error")
}

func TestGeneratedAPIResponseErrorAcceptsOK(t *testing.T) {
	resp := &generated.GetStatsResp{
		StatusCode: http.StatusOK,
		Body:       []byte(`{}`),
		JSON200:    &generated.GetStatsResponse{},
	}

	err := APIResponseError(resp, nil)

	require.NoError(t, err, "OK response")
}

func TestGeneratedResponseWrappersApplyStoreErrorMapping(t *testing.T) {
	require :=
		require.
			New(t)

	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	s := newTestStore(srv, "key")
	want := &generated.GetStatsResp{
		StatusCode: http.StatusOK,
		Body:       []byte(`{}`),
		JSON200:    &generated.GetStatsResponse{},
	}
	got, err := APIResponse(s, func(*apiclient.Client) (*generated.GetStatsResp, error) {
		return want, nil
	})
	require.NoError(
		err, "API wrapper")

	assert.Same(t, want, got, "API wrapper response")

	_, err = CLIResponse(s, func(*apiclient.Client) (*generated.CreateCLICollectionResp, error) {
		return &generated.CreateCLICollectionResp{
			StatusCode: http.StatusBadRequest,
			Body:       []byte(`{"error":"invalid_collection","message":"bad account"}`),
		}, nil
	})
	require.EqualError(err, "bad account", "CLI wrapper should keep bare message")

	transportErr := errors.New("connection refused")
	_, err = APIResponse(s, func(*apiclient.Client) (*generated.GetStatsResp, error) {
		return nil, transportErr
	})
	require.ErrorIs(err, transportErr, "transport error")
}

func TestDecodeGeneratedSearchBodyReportsOperation(t *testing.T) {
	_, err := DecodeGeneratedSearchBody[generated.SearchResult]("CLI hybrid search", []byte("{"))

	require.Error(t, err, "DecodeGeneratedSearchBody should reject malformed JSON")
	assert.ErrorContains(t, err, "decode CLI hybrid search response")
}

func TestGetStats_ErrorResponse(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"db_error","message":"database locked"}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	_, err := s.GetStats()
	require.Error(t, err, "GetStats should return error on 500")
	assert.ErrorContains(t, err, "database locked")
}

func TestGetStats_Success(t *testing.T) {
	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(generated.StatsResponse{
			TotalMessages:     100,
			TotalThreads:      50,
			TotalAccounts:     2,
			TotalLabels:       10,
			TotalAttachments:  5,
			DatabaseSizeBytes: 1024,
		})
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	stats, err := s.GetStats()
	require.NoError(t, err, "GetStats error")
	assert.Equal(int64(100), stats.MessageCount, "MessageCount")
	assert.Equal(int64(50), stats.ThreadCount, "ThreadCount")
	assert.Equal(int64(2), stats.SourceCount, "SourceCount")
}

func TestGetStatsUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/stats", r.URL.Path, "path")
		writeJSONResponse(t, w, generated.StatsResponse{
			TotalMessages:     100,
			TotalThreads:      50,
			TotalAccounts:     2,
			TotalLabels:       10,
			TotalAttachments:  5,
			DatabaseSizeBytes: 1024,
		})
	})

	stats, err := s.GetStats()
	require.NoError(t, err, "GetStats")
	assert.Equal(int64(100), stats.MessageCount, "MessageCount")
	assert.Equal(int64(50), stats.ThreadCount, "ThreadCount")
	assert.Equal(int64(2), stats.SourceCount, "SourceCount")
	assert.Equal(int64(10), stats.LabelCount, "LabelCount")
	assert.Equal(int64(5), stats.AttachmentCount, "AttachmentCount")
	assert.Equal(int64(1024), stats.DatabaseSize, "DatabaseSize")
}

func TestVectorSearchAvailable(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "status initializing is capable",
			body: `{"vector_status":"initializing"}`,
			want: true,
		},
		{
			name: "status ready is capable",
			body: `{"vector_status":"ready","vector_search":{"enabled":true}}`,
			want: true,
		},
		{
			name: "status stale is capable",
			body: `{"vector_status":"stale"}`,
			want: true,
		},
		{
			name: "status error is capable",
			body: `{"vector_status":"error"}`,
			want: true,
		},
		{
			name: "status disabled is not capable",
			body: `{"vector_status":"disabled"}`,
			want: false,
		},
		{
			name: "old daemon without status falls back to enabled flag",
			body: `{"vector_search":{"enabled":true}}`,
			want: true,
		},
		{
			name: "old daemon without status and disabled flag is not capable",
			body: `{"vector_search":{"enabled":false}}`,
			want: false,
		},
		{
			name: "old daemon without vector fields is not capable",
			body: `{}`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/stats", r.URL.Path, "path")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			s := newTestStore(srv, "key")
			got, err := s.VectorSearchAvailable(context.Background())
			require.NoError(t, err, "VectorSearchAvailable")
			assert.Equal(t, tt.want, got, "capable")
		})
	}
}

func TestGetCLIStats_Success(t *testing.T) {
	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/stats", r.URL.Path, "path")
		assert.Equal("Important", r.URL.Query().Get("collection"), "collection query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"stats": {
				"total_messages": 8,
				"total_threads": 6,
				"total_accounts": 2,
				"total_labels": 9,
				"total_attachments": 3,
				"database_size_bytes": 2097152
			},
			"scope_label": "Important",
			"scope_source_count": 2
		}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	stats, err := s.GetCLIStats(context.Background(), "", "Important")
	require.NoError(t, err, "GetCLIStats")

	require.NotNil(t, stats.Stats, "Stats")
	assert.Equal(int64(8), stats.Stats.MessageCount, "MessageCount")
	assert.Equal(int64(6), stats.Stats.ThreadCount, "ThreadCount")
	assert.Equal(int64(2), stats.Stats.SourceCount, "SourceCount")
	assert.Equal("Important", stats.ScopeLabel, "ScopeLabel")
	assert.Equal(2, stats.ScopeSourceCount, "ScopeSourceCount")
}

func TestGetCLISearch_Success(t *testing.T) {
	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/search", r.URL.Path, "path")
		assert.Equal("lunch", r.URL.Query().Get("q"), "query")
		assert.Equal("alice@example.com", r.URL.Query().Get("account"), "account query")
		assert.Equal("sms", r.URL.Query().Get("message_type"), "message_type query")
		assert.Equal("25", r.URL.Query().Get("limit"), "limit query")
		assert.Equal("5", r.URL.Query().Get("offset"), "offset query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"results": [{
				"id": 42,
				"source_message_id": "remote-42",
				"conversation_id": 7,
				"subject": "Lunch",
				"snippet": "see you there",
				"from_email": "alice@example.com",
				"sent_at": "2024-01-02T03:04:05Z",
				"size_estimate": 123,
				"has_attachments": true,
				"attachment_count": 1,
				"labels": ["INBOX"]
			}],
			"scope_label": "alice@example.com",
			"scope_source_count": 1
		}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	resp, err := s.GetCLISearch(
		context.Background(),
		CLISearchRequest{
			Query:        "lunch",
			Account:      "alice@example.com",
			MessageTypes: []string{"sms"},
			Limit:        25,
			Offset:       5,
		},
	)
	require.NoError(t, err, "GetCLISearch")

	require.Len(t, resp.Results, 1, "Results")
	assert.Equal(int64(42), resp.Results[0].ID, "result ID")
	assert.Equal("Lunch", resp.Results[0].Subject, "result Subject")
	assert.Equal("alice@example.com", resp.ScopeLabel, "ScopeLabel")
	assert.Equal(1, resp.ScopeSourceCount, "ScopeSourceCount")
}

func TestGetCLIHybridSearch_Success(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search", r.URL.Path, "path")
		assert.Equal("lunch", r.URL.Query().Get("q"), "query")
		assert.Equal("vector", r.URL.Query().Get("mode"), "mode query")
		assert.Equal("true", r.URL.Query().Get("explain"), "explain query")
		assert.Equal("Important", r.URL.Query().Get("collection"), "collection query")
		assert.Equal("sms", r.URL.Query().Get("message_type"), "message_type query")
		assert.Equal("25", r.URL.Query().Get("page_size"), "page_size query")
		assert.Equal("5", r.URL.Query().Get("offset"), "offset query")
		assert.Equal("true", r.URL.Query().Get("include_matches"), "include_matches query")
		assert.Equal("0.75", r.URL.Query().Get("min_score"), "min_score query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query": "lunch",
			"mode": "vector",
				"returned": 1,
				"pool_saturated": false,
				"has_more": true,
			"scope_label": "Important",
			"scope_source_count": 2,
			"took_ms": 12,
			"generation": {
				"id": 7,
				"model": "fake-model",
				"dimension": 4,
				"fingerprint": "fake:4",
				"state": "active"
			},
			"results": [{
				"id": 42,
				"subject": "Lunch",
				"from": "Alice <alice@example.com>",
				"from_email": "alice@example.com",
				"from_name": "Alice",
				"sent_at": "2024-01-02T03:04:05Z",
				"snippet": "Lunch tomorrow",
				"has_attachments": false,
				"labels": ["INBOX"],
				"to": ["bob@example.com"],
					"size_bytes": 512,
					"matches": [{
						"char_offset": 0,
						"line": 1,
						"snippet": "Lunch tomorrow",
						"score": 0.88
					}],
				"score": {
					"rrf": 0.5,
					"bm25": 1.25,
					"vector": 0.9,
					"subject_boosted": true
				}
			}]
		}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	resp, err := s.GetCLIHybridSearch(
		context.Background(),
		CLIHybridSearchRequest{
			Query:          "lunch",
			Collection:     "Important",
			MessageTypes:   []string{"sms"},
			Mode:           "vector",
			Limit:          25,
			Offset:         5,
			IncludeMatches: true,
			MinScore:       0.75,
		},
	)
	require.NoError(err, "GetCLIHybridSearch")

	require.Len(resp.Results, 1, "Results")
	assert.Equal(int64(42), resp.Results[0].ID, "result ID")
	assert.Equal("Lunch", resp.Results[0].Subject, "result Subject")
	assert.Equal("alice@example.com", resp.Results[0].FromEmail, "result FromEmail")
	assert.True(resp.Results[0].SubjectBoosted, "SubjectBoosted")
	if assert.NotNil(resp.Results[0].RRFScore, "RRFScore") {
		assert.InDelta(0.5, *resp.Results[0].RRFScore, 1e-9, "RRFScore")
	}
	if assert.NotNil(resp.Results[0].BM25Score, "BM25Score") {
		assert.InDelta(1.25, *resp.Results[0].BM25Score, 1e-9, "BM25Score")
	}
	if assert.NotNil(resp.Results[0].VectorScore, "VectorScore") {
		assert.InDelta(0.9, *resp.Results[0].VectorScore, 1e-9, "VectorScore")
	}
	assert.Equal(int64(7), resp.Generation.ID, "Generation.ID")
	assert.Equal("Important", resp.ScopeLabel, "ScopeLabel")
	assert.Equal(2, resp.ScopeSourceCount, "ScopeSourceCount")
	assert.True(resp.HasMore, "HasMore")
	require.Len(resp.Results[0].Matches, 1, "Matches")
	require.NotNil(resp.Results[0].Matches[0].CharOffset, "CharOffset")
	require.NotNil(resp.Results[0].Matches[0].Line, "Line")
	assert.Equal(0, *resp.Results[0].Matches[0].CharOffset, "CharOffset")
	assert.Equal(1, *resp.Results[0].Matches[0].Line, "Line")
	assert.InDelta(0.88, resp.Results[0].Matches[0].Score, 1e-9, "match Score")
}

func TestGetCLIHybridSearchUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search", r.URL.Path, "path")
		assert.Equal("lunch", r.URL.Query().Get("q"), "query")
		assert.Equal("hybrid", r.URL.Query().Get("mode"), "mode query")
		assert.Equal("true", r.URL.Query().Get("explain"), "explain query")
		assert.Equal("sms,mms", r.URL.Query().Get("message_type"), "message_type query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"query": "lunch",
			"mode": "hybrid",
			"returned": 1,
			"pool_saturated": false,
			"scope_source_count": 0,
			"took_ms": 12,
			"generation": {
				"id": 7,
				"model": "fake-model",
				"dimension": 4,
				"fingerprint": "fake:4",
				"state": "active"
			},
			"results": [{
				"id": 42,
				"subject": "Lunch",
				"from": "Alice <alice@example.com>",
				"from_email": "alice@example.com",
				"sent_at": "2024-01-02T03:04:05Z",
				"snippet": "Lunch tomorrow",
				"has_attachments": false,
				"labels": ["INBOX"],
				"to": ["bob@example.com"],
				"size_bytes": 512,
				"score": {
					"rrf": 0.5,
					"bm25": 1.25,
					"vector": 0.9,
					"subject_boosted": true
				}
			}]
		}`))
	})

	resp, err := s.GetCLIHybridSearch(
		context.Background(),
		CLIHybridSearchRequest{
			Query:        "lunch",
			MessageTypes: []string{"sms", "mms"},
			Mode:         "hybrid",
			Limit:        25,
		},
	)
	require.NoError(err, "GetCLIHybridSearch")
	require.Len(resp.Results, 1, "Results")
	assert.Equal("alice@example.com", resp.Results[0].FromEmail, "result FromEmail")
}

func TestGetCLIAccounts_Success(t *testing.T) {
	require :=
		require.
			New(t)

	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/accounts", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"accounts": [{
				"id": 7,
				"email": "alice@example.com",
				"type": "gmail",
				"display_name": "Alice",
				"message_count": 1234,
				"last_sync": "2024-01-02T03:04:05Z"
			}]
		}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	accounts, err := s.GetCLIAccounts(context.Background())
	require.NoError(
		err, "GetCLIAccounts")

	require.Len(accounts, 1, "accounts")
	assert.Equal(int64(7), accounts[0].ID, "ID")
	assert.Equal("alice@example.com", accounts[0].Email, "Email")
	assert.Equal("gmail", accounts[0].Type, "Type")
	assert.Equal("Alice", accounts[0].DisplayName, "DisplayName")
	assert.Equal(int64(1234), accounts[0].MessageCount, "MessageCount")
	require.NotNil(accounts[0].LastSync, "LastSync")
	assert.Equal("2024-01-02T03:04:05Z", accounts[0].LastSync.UTC().Format(time.RFC3339), "LastSync")
}

func TestCLICollectionMutations(t *testing.T) {
	require :=
		require.
			New(t)

	assert := assert.New(t)
	collectionName := "Team/One"
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/cli/collections":
			var req CLICollectionCreateRequest
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&req), "decode create request") {
				http.Error(w, "bad create request", http.StatusBadRequest)
				return
			}
			assert.Equal(collectionName, req.Name, "create name")
			assert.Equal([]string{"alice@example.com", "bob@example.com"}, req.Accounts, "create accounts")
			_, _ = w.Write([]byte(`{"name":"Team/One","source_count":2}`))
		case r.Method == http.MethodPatch && r.RequestURI == "/api/v1/cli/collections/Team%2FOne/sources":
			var req CLICollectionSourcesRequest
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&req), "decode add request") {
				http.Error(w, "bad add request", http.StatusBadRequest)
				return
			}
			assert.Equal([]string{"alice@example.com"}, req.Accounts, "add accounts")
			_, _ = w.Write([]byte(`{"name":"Team/One","source_count":1}`))
		case r.Method == http.MethodDelete && r.RequestURI == "/api/v1/cli/collections/Team%2FOne/sources":
			var req CLICollectionSourcesRequest
			if !assert.NoError(json.NewDecoder(r.Body).Decode(&req), "decode remove request") {
				http.Error(w, "bad remove request", http.StatusBadRequest)
				return
			}
			assert.Equal([]string{"bob@example.com"}, req.Accounts, "remove accounts")
			_, _ = w.Write([]byte(`{"name":"Team/One","source_count":1}`))
		case r.Method == http.MethodDelete && r.RequestURI == "/api/v1/cli/collections/Team%2FOne":
			_, _ = w.Write([]byte(`{"name":"Team/One"}`))
		default:
			http.Error(w, "unexpected collection mutation request", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	createResult, err := s.CreateCLICollection(context.Background(), CLICollectionCreateRequest{
		Name:     collectionName,
		Accounts: []string{"alice@example.com", "bob@example.com"},
	})
	require.NoError(
		err, "CreateCLICollection")

	assert.Equal(collectionName, createResult.Name, "create result name")
	assert.Equal(2, createResult.SourceCount, "create source count")

	addResult, err := s.AddCLICollectionSources(context.Background(), collectionName, CLICollectionSourcesRequest{
		Accounts: []string{"alice@example.com"},
	})
	require.NoError(
		err, "AddCLICollectionSources")

	assert.Equal(collectionName, addResult.Name, "add result name")
	assert.Equal(1, addResult.SourceCount, "add source count")

	removeResult, err := s.RemoveCLICollectionSources(context.Background(), collectionName, CLICollectionSourcesRequest{
		Accounts: []string{"bob@example.com"},
	})
	require.NoError(
		err, "RemoveCLICollectionSources")

	assert.Equal(collectionName, removeResult.Name, "remove result name")
	assert.Equal(1, removeResult.SourceCount, "remove source count")

	deleteResult, err := s.DeleteCLICollection(context.Background(), collectionName)
	require.NoError(
		err, "DeleteCLICollection")

	assert.Equal(collectionName, deleteResult.Name, "delete result name")
}

func TestGetCLICollection_NotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/cli/collection", r.URL.Path, "path")
		assert.Equal(t, "missing", r.URL.Query().Get("name"), "name query")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"Collection not found"}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	collection, err := s.GetCLICollection(context.Background(), "missing")

	require.ErrorIs(t, err, store.ErrCollectionNotFound, "GetCLICollection(missing)")
	assert.Nil(t, collection, "collection")
}

func TestRebuildCLIFTSStreamsProgress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newCLINDJSONTestServer(t, http.MethodPost, "/api/v1/cli/rebuild-fts",
		`{"type":"progress","done":2,"total":4}`,
		`{"type":"progress","done":4,"total":4}`,
		`{"type":"complete","indexed":3}`,
	)
	defer srv.Close()

	s := newTestStore(srv, "secret")
	var progress [][2]int64
	indexed, err := s.RebuildCLIFTS(context.Background(), func(done, total int64) {
		progress = append(progress, [2]int64{done, total})
	})

	require.NoError(err, "RebuildCLIFTS")
	assert.Equal(int64(3), indexed, "indexed")
	assert.Equal([][2]int64{{2, 4}, {4, 4}}, progress, "progress")
}

func TestRebuildCLIFTSIgnoresUnknownStreamEvents(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newCLINDJSONTestServer(t, http.MethodPost, "/api/v1/cli/rebuild-fts",
		`{"type":"notice","message":"future daemon detail"}`,
		`{"type":"progress","done":1,"total":2}`,
		`{"type":"complete","indexed":2}`,
	)
	defer srv.Close()

	s := newTestStore(srv, "secret")
	var progress [][2]int64
	indexed, err := s.RebuildCLIFTS(context.Background(), func(done, total int64) {
		progress = append(progress, [2]int64{done, total})
	})

	require.NoError(err, "RebuildCLIFTS")
	assert.Equal(int64(2), indexed, "indexed")
	assert.Equal([][2]int64{{1, 2}}, progress, "progress")
}

func TestRunCLIRepairEncodingStreamsOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := newCLINDJSONTestServer(t, http.MethodPost, "/api/v1/cli/repair-encoding",
		`{"type":"stdout","data":"Scanning messages\n"}`,
		`{"type":"stderr","data":"repair warning\n"}`,
		`{"type":"complete"}`,
	)
	defer srv.Close()

	s := newTestStore(srv, "secret")
	var output []string
	err := s.RunCLIRepairEncoding(context.Background(), func(stream, data string) error {
		output = append(output, stream+":"+data)
		return nil
	})

	require.NoError(err, "RunCLIRepairEncoding")
	assert.Equal([]string{"stdout:Scanning messages\n", "stderr:repair warning\n"}, output, "output")
}

func TestRebuildCLIFTSUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/rebuild-fts", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"complete","indexed":5}` + "\n"))
	})

	indexed, err := s.RebuildCLIFTS(context.Background(), nil)

	require.NoError(err, "RebuildCLIFTS")
	assert.Equal(int64(5), indexed, "indexed")
}

func TestGetCLIIdentities_Success(t *testing.T) {
	require :=
		require.
			New(t)

	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/identities", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("account"), "account query")
		assert.Equal("true", r.URL.Query().Get("primary_only"), "primary_only query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"rows": [{
				"account": "alice@example.com",
				"source_id": 7,
				"source_type": "gmail",
				"identifier": "alice@example.com",
				"signals": ["manual"],
				"confirmed_at": "2024-01-02T03:04:05Z"
			}, {
				"account": "old-mbox",
				"source_id": 8,
				"source_type": "mbox",
				"signals": [],
				"none": true
			}]
		}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	rows, err := s.GetCLIIdentities(context.Background(), CLIIdentitiesRequest{
		Account:     "alice@example.com",
		PrimaryOnly: true,
	})
	require.NoError(
		err, "GetCLIIdentities")

	require.Len(rows, 2, "rows")
	assert.Equal("alice@example.com", rows[0].Account, "identity account")
	assert.Equal(int64(7), rows[0].SourceID, "identity source ID")
	assert.Equal("gmail", rows[0].SourceType, "identity source type")
	assert.Equal("alice@example.com", rows[0].Identifier, "identity identifier")
	assert.Equal([]string{"manual"}, rows[0].Signals, "identity signals")
	require.NotNil(rows[0].ConfirmedAt, "identity confirmed_at")
	assert.Equal("2024-01-02T03:04:05Z", rows[0].ConfirmedAt.UTC().Format(time.RFC3339), "identity confirmed_at")
	assert.False(rows[0].None, "identity none")

	assert.Equal("old-mbox", rows[1].Account, "none account")
	assert.Equal(int64(8), rows[1].SourceID, "none source ID")
	assert.Equal("mbox", rows[1].SourceType, "none source type")
	assert.Empty(rows[1].Identifier, "none identifier")
	assert.Empty(rows[1].Signals, "none signals")
	assert.Nil(rows[1].ConfirmedAt, "none confirmed_at")
	assert.True(rows[1].None, "none flag")
}

func TestGetCLIMessage_Success(t *testing.T) {
	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/message", r.URL.Path, "path")
		assert.Equal("remote-42", r.URL.Query().Get("id"), "id query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": 42,
			"source_message_id": "remote-42",
			"conversation_id": 7,
			"subject": "Test Subject",
			"snippet": "short",
			"sent_at": "2024-01-02T03:04:05Z",
			"size_estimate": 512,
			"has_attachments": false,
			"from": [{"email": "alice@example.com", "name": "Alice"}],
			"to": [{"email": "bob@example.com", "name": "Bob"}],
			"labels": ["INBOX"],
			"attachments": [],
			"body_text": "Hello over HTTP"
		}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	msg, err := s.GetCLIMessage(context.Background(), "remote-42")
	require.NoError(t, err, "GetCLIMessage")

	require.NotNil(t, msg, "message")
	assert.Equal(int64(42), msg.ID, "ID")
	assert.Equal("remote-42", msg.SourceMessageID, "SourceMessageID")
	assert.Equal("Test Subject", msg.Subject, "Subject")
	assert.Equal("alice@example.com", msg.From[0].Email, "From")
	assert.Equal("Hello over HTTP", msg.BodyText, "BodyText")
}

func TestGetCLIMessage_NotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"Message not found"}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	_, err := s.GetCLIMessage(context.Background(), "missing")
	require.ErrorIs(t, err, store.ErrMessageNotFound, "GetCLIMessage(missing)")
}

func TestGetCLIMessageRaw_Success(t *testing.T) {
	assert := assert.New(t)
	raw := []byte("From: alice@example.com\r\nSubject: Raw\r\n\r\nBody")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/message/raw", r.URL.Path, "path")
		assert.Equal("gmail-42", r.URL.Query().Get("id"), "id query")
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("X-Msgvault-Source-Message-Id", "gmail-42")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	got, sourceMessageID, err := s.GetCLIMessageRaw(context.Background(), "gmail-42")
	require.NoError(t, err, "GetCLIMessageRaw")

	assert.Equal(raw, got, "raw")
	assert.Equal("gmail-42", sourceMessageID, "SourceMessageID")
}

func TestGetCLIMessageRawUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("From: alice@example.com\r\nSubject: Raw\r\n\r\nBody")

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/message/raw", r.URL.Path, "path")
		assert.Equal("gmail-42", r.URL.Query().Get("id"), "id query")
		w.Header().Set("Content-Type", "message/rfc822")
		w.Header().Set("X-Msgvault-Source-Message-Id", "gmail-42")
		_, _ = w.Write(raw)
	})

	got, sourceMessageID, err := s.GetCLIMessageRaw(context.Background(), "gmail-42")
	require.NoError(err, "GetCLIMessageRaw")
	assert.Equal(raw, got, "raw")
	assert.Equal("gmail-42", sourceMessageID, "SourceMessageID")
}

func TestGetCLIMessageRaw_NotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found","message":"Message not found"}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	_, _, err := s.GetCLIMessageRaw(context.Background(), "missing")
	require.ErrorIs(t, err, store.ErrMessageNotFound, "GetCLIMessageRaw(missing)")
}

func TestGetCLIMessageRaw_NotFoundUsesStableErrorCode(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"message_not_found","message":"No message matched that id"}`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	_, _, err := s.GetCLIMessageRaw(context.Background(), "missing")
	require.ErrorIs(t, err, store.ErrMessageNotFound, "GetCLIMessageRaw(missing)")
}

func TestGetCLIMessageRaw_NotFoundPreservesGeneratedDecodeError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":`))
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	_, _, err := s.GetCLIMessageRaw(context.Background(), "missing")

	var decodeErr *runtime.ResponseDecodeError
	require.ErrorAs(t, err, &decodeErr, "GetCLIMessageRaw malformed 404 should preserve generated decode error")
	assert.NotContains(t, err.Error(), "API error (404)", "decode failures are contract errors, not daemon API messages")
}

func TestGetCLIAttachment_Success(t *testing.T) {
	assert := assert.New(t)
	data := []byte("attachment bytes")
	contentHash := fmt.Sprintf("%x", sha256.Sum256(data))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/attachment", r.URL.Path, "path")
		assert.Equal(contentHash, r.URL.Query().Get("content_hash"), "content_hash query")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	got, err := s.GetCLIAttachment(context.Background(), contentHash)
	require.NoError(t, err, "GetCLIAttachment")

	assert.Equal(data, got, "data")
}

func TestGetCLIAttachmentUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	data := []byte("attachment bytes")
	contentHash := fmt.Sprintf("%x", sha256.Sum256(data))

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/attachment", r.URL.Path, "path")
		assert.Equal(contentHash, r.URL.Query().Get("content_hash"), "content_hash query")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	})

	got, err := s.GetCLIAttachment(context.Background(), contentHash)
	require.NoError(err, "GetCLIAttachment")
	assert.Equal(data, got, "data")
}

func TestOpenCLIAttachment_Success(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	data := []byte("attachment bytes")
	contentHash := fmt.Sprintf("%x", sha256.Sum256(data))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/attachment", r.URL.Path, "path")
		assert.Equal(contentHash, r.URL.Query().Get("content_hash"), "content_hash query")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	body, err := s.OpenCLIAttachment(context.Background(), contentHash)
	require.NoError(err, "OpenCLIAttachment")
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	require.NoError(err, "read stream")
	assert.Equal(data, got, "data")
}

func TestOpenCLIAttachmentUsesGeneratedClientAdapter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	data := []byte("attachment bytes")
	contentHash := fmt.Sprintf("%x", sha256.Sum256(data))

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/attachment", r.URL.Path, "path")
		assert.Equal(contentHash, r.URL.Query().Get("content_hash"), "content_hash query")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	})

	body, err := s.OpenCLIAttachment(context.Background(), contentHash)
	require.NoError(err, "OpenCLIAttachment")
	defer func() { _ = body.Close() }()

	got, err := io.ReadAll(body)
	require.NoError(err, "read stream")
	assert.Equal(data, got, "data")
}

func TestGetCLIAttachmentRejectsSameLengthCorruption(t *testing.T) {
	want := []byte("expected attachment")
	corrupt := bytes.Clone(want)
	corrupt[0] ^= 0xff
	contentHash := fmt.Sprintf("%x", sha256.Sum256(want))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(corrupt)
	}))
	t.Cleanup(srv.Close)

	s := newTestStore(srv, "key")
	_, err := s.GetCLIAttachment(context.Background(), contentHash)
	require.ErrorIs(t, err, contentverify.ErrMismatch)
}

func TestOpenCLIAttachmentRejectsSameLengthCorruption(t *testing.T) {
	want := []byte("expected attachment")
	corrupt := bytes.Clone(want)
	corrupt[len(corrupt)-1] ^= 0xff
	contentHash := fmt.Sprintf("%x", sha256.Sum256(want))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(corrupt)
	}))
	t.Cleanup(srv.Close)

	s := newTestStore(srv, "key")
	body, err := s.OpenCLIAttachment(context.Background(), contentHash)
	require.NoError(t, err)
	_, readErr := io.ReadAll(body)
	require.ErrorIs(t, readErr, contentverify.ErrMismatch)
	require.ErrorIs(t, body.Close(), contentverify.ErrMismatch)
}

func TestGetMessage_NotFound(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	msg, err := s.GetMessage(999)
	require.ErrorIs(t, err, store.ErrMessageNotFound, "GetMessage(999) should report not found")
	assert.Nil(t, msg, "GetMessage(999) should return nil for not found")
}

func TestGetMessage_Success(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/42", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(generated.MessageDetail{
			ID:      42,
			Subject: "Test Subject",
			From:    "sender@example.com",
			To:      []string{"receiver@example.com"},
			SentAt:  "2024-01-15T10:30:00Z",
			Snippet: "preview",
			Labels:  []string{"INBOX"},
			Body:    "Hello, world!",
			Attachments: []generated.AttachmentInfo{
				{Filename: "doc.pdf", MimeType: "application/pdf", SizeBytes: 1024},
			},
		})
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	msg, err := s.GetMessage(42)
	require.NoError(err, "GetMessage error")
	require.NotNil(msg, "GetMessage returned nil")
	assert.Equal("Test Subject", msg.Subject, "Subject")
	assert.Equal("Hello, world!", msg.Body, "Body")
	require.Len(msg.Attachments, 1, "len(Attachments)")
	assert.Equal("doc.pdf", msg.Attachments[0].Filename, "Attachments[0].Filename")
}

func TestGetMessageUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/42", r.URL.Path, "path")
		writeJSONResponse(t, w, generated.MessageDetail{
			ID:             42,
			ConversationID: int64Ptr(7),
			Subject:        "Generated detail",
			MessageType:    stringPtr("email"),
			From:           "sender@example.com",
			To:             []string{"receiver@example.com"},
			SentAt:         "2024-01-15T10:30:00Z",
			DeletedAt:      stringPtr("2026-03-18T15:00:00Z"),
			Snippet:        "preview",
			Labels:         []string{"INBOX"},
			HasAttachments: true,
			SizeBytes:      2048,
			Body:           "Hello, generated world!",
			Attachments: []generated.AttachmentInfo{
				{
					ID:          52,
					Filename:    "doc.pdf",
					MimeType:    "application/pdf",
					SizeBytes:   1024,
					ContentHash: stringPtr("hash-123"),
					URL:         stringPtr("/api/v1/attachments/hash-123"),
				},
			},
		})
	})

	msg, err := s.GetMessage(42)
	require.NoError(
		err, "GetMessage")

	require.NotNil(msg, "GetMessage returned nil")
	assert.Equal(int64(42), msg.ID, "ID")
	assert.Equal(int64(7), msg.ConversationID, "ConversationID")
	assert.Equal("Generated detail", msg.Subject, "Subject")
	assert.Equal("email", msg.MessageType, "MessageType")
	assert.Equal("Hello, generated world!", msg.Body, "Body")
	require.Len(msg.Attachments, 1, "len(Attachments)")
	assert.Equal(int64(52), msg.Attachments[0].ID, "Attachments[0].ID")
	assert.Equal("doc.pdf", msg.Attachments[0].Filename, "Attachments[0].Filename")
	assert.Equal("hash-123", msg.Attachments[0].ContentHash, "Attachments[0].ContentHash")
	require.NotNil(msg.DeletedAt, "DeletedAt")
	assert.Equal("2026-03-18T15:00:00Z", msg.DeletedAt.UTC().Format(time.RFC3339), "DeletedAt")
}

func TestListMessages_ZeroLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps := r.URL.Query().Get("page_size")
		assert.NotEqual(t, "0", ps, "page_size should not be 0")
		resp := generated.MessageListResponse{
			Total:    0,
			Page:     1,
			PageSize: 20,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	s := newTestStore(srv, "test")

	// This previously panicked with divide-by-zero
	msgs, total, err := s.ListMessages(0, 0)
	require.NoError(t, err, "ListMessages(0, 0) error")
	assert.Equal(t, int64(0), total, "total")
	assert.Empty(t, msgs, "len(msgs)")
}

func TestListMessages_NegativeLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps := r.URL.Query().Get("page_size")
		assert.Equal(t, "20", ps, "page_size should default to 20")
		resp := generated.MessageListResponse{Total: 0, Page: 1, PageSize: 20}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	s := newTestStore(srv, "test")

	_, _, err := s.ListMessages(0, -5)
	require.NoError(t, err, "ListMessages(0, -5) error")
}

func TestListMessagesUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages", r.URL.Path, "path")
		assert.Equal("3", r.URL.Query().Get("page"), "page")
		assert.Equal("20", r.URL.Query().Get("page_size"), "page_size")
		writeJSONResponse(t, w, generated.MessageListResponse{
			Total:    55,
			Page:     3,
			PageSize: 20,
			Messages: []generated.MessageSummary{
				generatedMessageSummaryFixture("Generated message"),
			},
		})
	})

	msgs, total, err := s.ListMessages(40, 20)
	require.NoError(
		err, "ListMessages")

	assert.Equal(int64(55), total, "total")
	require.Len(msgs, 1, "len(msgs)")
	assert.Equal(int64(42), msgs[0].ID, "ID")
	assert.Equal(int64(7), msgs[0].ConversationID, "ConversationID")
	assert.Equal("Generated message", msgs[0].Subject, "Subject")
	assert.Equal("sms", msgs[0].MessageType, "MessageType")
	assert.Equal("Alice <alice@example.com>", msgs[0].From, "From")
	assert.Equal("msg-42", msgs[0].SourceMessageID, "SourceMessageID")
	assert.Equal("alice@example.com", msgs[0].FromEmail, "FromEmail")
	assert.Equal("Alice", msgs[0].FromName, "FromName")
	assert.Equal("+15555550123", msgs[0].FromPhone, "FromPhone")
	assert.Equal([]string{"carol@example.com"}, msgs[0].Cc, "Cc")
	assert.Equal([]string{"dave@example.com"}, msgs[0].Bcc, "Bcc")
	assert.True(msgs[0].HasAttachments, "HasAttachments")
	assert.Equal(int64(1234), msgs[0].SizeEstimate, "SizeEstimate")
	require.NotNil(msgs[0].DeletedAt, "DeletedAt")
	assert.Equal("2026-03-18T15:00:00Z", msgs[0].DeletedAt.UTC().Format(time.RFC3339), "DeletedAt")
}

func TestSearchMessages_ZeroLimit(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ps := r.URL.Query().Get("page_size")
		assert.NotEqual(t, "0", ps, "page_size should not be 0")
		resp := generated.SearchResult{
			Query:    "test",
			Total:    0,
			Page:     1,
			PageSize: 20,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	s := newTestStore(srv, "test")

	// This previously panicked with divide-by-zero
	msgs, total, err := s.SearchMessages("test", 0, 0)
	require.NoError(t, err, "SearchMessages(test, 0, 0) error")
	assert.Equal(t, int64(0), total, "total")
	assert.Empty(t, msgs, "len(msgs)")
}

func TestSearchMessages_QueryEncoding(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		assert.Equal(t, "hello world", q, "q")
		resp := generated.SearchResult{Query: "hello world", Total: 0, Page: 1, PageSize: 20}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	s := newTestStore(srv, "test")
	_, _, err := s.SearchMessages("hello world", 0, 20)
	require.NoError(t, err, "SearchMessages error")
}

func TestSearchMessagesUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/search", r.URL.Path, "path")
		assert.Equal("hello world", r.URL.Query().Get("q"), "q")
		assert.Equal("3", r.URL.Query().Get("page"), "page")
		assert.Equal("20", r.URL.Query().Get("page_size"), "page_size")
		writeJSONResponse(t, w, generated.SearchResult{
			Query:    "hello world",
			Total:    55,
			Page:     3,
			PageSize: 20,
			Messages: []generated.MessageSummary{
				generatedMessageSummaryFixture("Generated search hit"),
			},
		})
	})

	msgs, total, err := s.SearchMessages("hello world", 40, 20)
	require.NoError(
		err, "SearchMessages")

	assert.Equal(int64(55), total, "total")
	require.Len(msgs, 1, "len(msgs)")
	assert.Equal(int64(42), msgs[0].ID, "ID")
	assert.Equal(int64(7), msgs[0].ConversationID, "ConversationID")
	assert.Equal("Generated search hit", msgs[0].Subject, "Subject")
	assert.Equal("sms", msgs[0].MessageType, "MessageType")
	assert.Equal("msg-42", msgs[0].SourceMessageID, "SourceMessageID")
	assert.Equal("alice@example.com", msgs[0].FromEmail, "FromEmail")
	assert.Equal("Alice", msgs[0].FromName, "FromName")
	assert.Equal("+15555550123", msgs[0].FromPhone, "FromPhone")
	assert.Equal([]string{"carol@example.com"}, msgs[0].Cc, "Cc")
	assert.Equal([]string{"dave@example.com"}, msgs[0].Bcc, "Bcc")
	assert.True(msgs[0].HasAttachments, "HasAttachments")
	assert.Equal(int64(1234), msgs[0].SizeEstimate, "SizeEstimate")
	require.NotNil(msgs[0].DeletedAt, "DeletedAt")
	assert.Equal("2026-03-18T15:00:00Z", msgs[0].DeletedAt.UTC().Format(time.RFC3339), "DeletedAt")
}

func TestListMessages_PageCalculation(t *testing.T) {
	tests := []struct {
		name     string
		offset   int
		limit    int
		wantPage string
		wantSize string
	}{
		{"first page", 0, 20, "1", "20"},
		{"second page", 20, 20, "2", "20"},
		{"third page", 40, 20, "3", "20"},
		{"small pages", 10, 10, "2", "10"},
		{"zero limit defaults", 0, 0, "1", "20"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				ps := r.URL.Query().Get("page_size")
				assert.Equal(t, tt.wantPage, page, "page")
				assert.Equal(t, tt.wantSize, ps, "page_size")
				resp := generated.MessageListResponse{Total: 0, Page: 1, PageSize: 20}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			s := newTestStore(srv, "test")

			_, _, err := s.ListMessages(tt.offset, tt.limit)
			require.NoError(t, err, "ListMessages(%d, %d) error", tt.offset, tt.limit)
		})
	}
}

func TestListAccounts_Success(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/accounts", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(generated.AccountListResponse{
			Accounts: []generated.AccountInfo{
				{Email: "user@gmail.com", Enabled: true, Schedule: stringPtr("0 2 * * *")},
			},
		})
	}))
	defer srv.Close()

	s := newTestStore(srv, "key")
	accounts, err := s.ListAccounts()
	require.NoError(t, err, "ListAccounts error")
	require.Len(t, accounts, 1, "len(accounts)")
	assert.Equal(t, "user@gmail.com", accounts[0].Email, "Email")
}

func TestListAccountsUsesGeneratedClientAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	s := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/accounts", r.URL.Path, "path")
		writeJSONResponse(t, w, generated.AccountListResponse{
			Accounts: []generated.AccountInfo{
				{
					ID:          42,
					Email:       "user@gmail.com",
					DisplayName: stringPtr("User"),
					LastSyncAt:  stringPtr("2026-06-29T15:30:00Z"),
					NextSyncAt:  stringPtr("2026-06-30T15:30:00Z"),
					Schedule:    stringPtr("0 2 * * *"),
					Enabled:     true,
				},
			},
		})
	})

	accounts, err := s.ListAccounts()
	require.NoError(
		err, "ListAccounts")

	require.Len(accounts, 1, "len(accounts)")
	assert.Equal(int64(42), accounts[0].ID, "ID")
	assert.Equal("user@gmail.com", accounts[0].Email, "Email")
	assert.Equal("User", accounts[0].DisplayName, "DisplayName")
	assert.Equal("2026-06-29T15:30:00Z", accounts[0].LastSyncAt, "LastSyncAt")
	assert.Equal("2026-06-30T15:30:00Z", accounts[0].NextSyncAt, "NextSyncAt")
	assert.Equal("0 2 * * *", accounts[0].Schedule, "Schedule")
	assert.True(accounts[0].Enabled, "Enabled")
}

// readCloser wraps a string in an io.ReadCloser. The embedded *strings.Reader
// promotes Read so io.EOF propagates verbatim to callers.
func readCloser(s string) *readCloserImpl {
	return &readCloserImpl{Reader: strings.NewReader(s)}
}

type readCloserImpl struct {
	*strings.Reader
}

func (rc *readCloserImpl) Close() error {
	return nil
}
