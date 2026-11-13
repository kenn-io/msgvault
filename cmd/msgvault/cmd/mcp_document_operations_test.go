package cmd

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type documentScheduler struct {
	api.SyncScheduler

	startErr error
	starts   int
}

func (s *documentScheduler) IsJobScheduled(name string) bool {
	return name == api.DocbankAttachmentMirrorJob
}
func (s *documentScheduler) StartJob(_ string, _ bool) (scheduler.JobDisposition, error) {
	s.starts++
	return "", s.startErr
}
func (s *documentScheduler) JobStatus() []api.JobStatus {
	return []api.JobStatus{{Name: api.DocbankAttachmentMirrorJob, LastError: "secret diagnostic /private/path"}}
}

func TestMCPDocumentMirrorNativeRetryAndProjection(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	cfg.Integrations.Docbank.DocumentProfile = "documents"
	hash := strings.Repeat("a", 64)
	requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), f.CreateMessage("mirror"), store.AttachmentWrite{ContentHash: hash, Size: 20, StoragePath: "aa/" + hash, State: "stored"}))
	requirements.NoError(f.Store.RecordDocbankAttachment(t.Context(), "destination", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{}, "source_unavailable"))
	sched := &documentScheduler{}
	server := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: f.Store, Scheduler: sched, Logger: slog.New(slog.DiscardHandler), DocbankAttachments: &api.DocbankAttachmentReader{Store: f.Store, Destination: "destination", Folder: "/private/path"}})
	backend := sourceOperationFixture(t, server)
	var disclosure string
	approve := false
	session := loopOperationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyDocuments}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		disclosure = request.Params.Message
		action := "decline"
		if approve {
			action = "accept"
		}
		return &sdkmcp.ElicitResult{Action: action, Content: map[string]any{"confirm": approve}}, nil
	}})
	call := func(t *testing.T, name string) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		require.NoError(t, err)
		return result
	}
	t.Run("projection", func(t *testing.T) {
		requirements, assertions := require.New(t), assert.New(t)
		status := loopToolOutput[map[string]any](t, call(t, "get_docbank_attachment_status"))
		assertions.Equal("running", status["state"])
		assertions.NotContains(status, "has_last_error")
		counts, ok := status["counts"].(map[string]any)
		requirements.True(ok)
		assertions.InDelta(1, counts["failed"], 0)
		assertions.NotContains(counts, "reasons")
		assertions.NotContains(status, "folder")
		assertions.NotContains(status, "last_error")
		processing, ok := status["processing"].(map[string]any)
		requirements.True(ok)
		assertions.NotContains(processing, "outcomes")

		// A newer daemon may add states or return an unfamiliar retry receipt.
		var statusBody, receiptBody atomic.Value
		statusBody.Store(`{"state":"running","upload_consent":true,"folder":"/private","processing":{"waiting":1,"requested":2,"extracted":3,"outcomes":{"operator_required":4,"other_reason":5}}}`)
		receiptBody.Store(`{"status":"scheduled"}`)
		future := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				body, ok := receiptBody.Load().(string)
				if !ok {
					http.Error(w, "invalid fixture", http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(body))
				return
			}
			body, ok := statusBody.Load().(string)
			if !ok {
				http.Error(w, "invalid fixture", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(future.Close)
		client, err := daemonclient.New(daemonclient.Config{URL: future.URL, AllowInsecure: true})
		requirements.NoError(err)
		newer := &daemonMCPOperations{client: client, supported: []string{"get_docbank_attachment_status", "retry_docbank_attachment_mirror"}}
		result, err := newer.ExecuteOperation(t.Context(), "get_docbank_attachment_status", nil)
		requirements.NoError(err)
		projected := operationOutput[mcpserver.DocumentMirrorStatus](t, result)
		requirements.NotNil(projected.Processing)
		assertions.Equal(mcpserver.DocumentProcessingCounts{Waiting: 1, Requested: 2, Extracted: 3, Other: 9}, *projected.Processing)
		statusBody.Store(`{"state":"future_state","upload_consent":true,"folder":"/private"}`)
		result, err = newer.ExecuteOperation(t.Context(), "get_docbank_attachment_status", nil)
		requirements.NoError(err)
		assertLoopRefusal(t, result, "invalid_operation_response", false)
		for _, body := range []string{`{"status":"future_status"}`, `{}`} {
			receiptBody.Store(body)
			result, err = newer.ExecuteOperation(t.Context(), "retry_docbank_attachment_mirror", nil)
			requirements.NoError(err)
			assertLoopRefusal(t, result, "invalid_operation_response", true)
		}
	})
	t.Run("retry and failure", func(t *testing.T) {
		requirements, assertions := require.New(t), assert.New(t)
		requirements.NoError(f.Store.RecordDocbankAttachmentProcessing(t.Context(), "destination", store.DocbankAttachmentProcessing{ContentHash: hash, Fingerprint: "synthetic", Outcome: "operator_required"}, time.Time{}))
		assertions.True(call(t, "retry_docbank_attachment_mirror").IsError)
		assertions.Zero(sched.starts)
		var processingRows int
		requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM docbank_attachment_processing").Scan(&processingRows))
		assertions.Equal(1, processingRows)
		due, err := f.Store.SendingDocbankAttachments(t.Context(), "destination")
		requirements.NoError(err)
		assertions.Empty(due)
		approve = true
		receipt := loopToolOutput[generated.DocbankAttachmentBackfillResponse](t, call(t, "retry_docbank_attachment_mirror"))
		assertions.Equal("scheduled", receipt.Status)
		requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM docbank_attachment_processing").Scan(&processingRows))
		assertions.Zero(processingRows)
		assertions.Equal("Retry failed external attachment mirror uploads, re-request recoverable document processing, and restart full discovery even when no uploads have failed. Existing native upload and processing consent apply. Scheduling is not completion.", disclosure)
		due, err = f.Store.SendingDocbankAttachments(t.Context(), "destination")
		requirements.NoError(err)
		assertions.Len(due, 1)
		// Exercise the native consent refusal by changing the shared config in this fixture.
		cfg.Integrations.Docbank.AttachmentUploadConsent = false
		off := loopToolOutput[map[string]any](t, call(t, "get_docbank_attachment_status"))
		assertions.Equal("consent_off", off["state"])
		assertions.NotContains(off, "counts")
		assertions.NotContains(off, "processing")
		failure := loopToolFailure(t, call(t, "retry_docbank_attachment_mirror"))
		assertions.Equal("conflict", failure["error"])
		assertions.Equal(false, failure["operation_may_have_completed"])
		cfg.Integrations.Docbank.AttachmentUploadConsent = true
		requirements.NoError(f.Store.RecordDocbankAttachment(t.Context(), "destination", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{}, "source_unavailable"))
		sched.startErr = errors.New("private scheduling diagnostic")
		failure = loopToolFailure(t, call(t, "retry_docbank_attachment_mirror"))
		assertions.Equal("operation_refused", failure["error"])
		assertions.Equal(true, failure["operation_may_have_completed"])
		due, err = f.Store.SendingDocbankAttachments(t.Context(), "destination")
		requirements.NoError(err)
		assertions.Len(due, 1)
	})
}

func TestMCPDocumentCapabilitiesFollowRoutes(t *testing.T) {
	for _, tc := range []struct{ id, method, path, want string }{
		{"getDocbankAttachmentStatus", "GET", "/api/v1/integrations/docbank/attachments", "get_docbank_attachment_status"},
		{"backfillDocbankAttachments", "POST", "/api/v1/integrations/docbank/attachments/backfill", "retry_docbank_attachment_mirror"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			assertions := assert.New(t)
			capabilities := &apiprotocol.MCPCapabilities{Routes: []apiprotocol.MCPRouteDescriptor{{OperationID: tc.id, Method: tc.method, Path: tc.path}}}
			assertions.Equal([]string{tc.want}, newDaemonMCPOperations(nil, capabilities).capabilities())
			capabilities.Routes[0].Method = "DELETE"
			assertions.Empty(newDaemonMCPOperations(nil, capabilities).capabilities())
		})
	}
}
