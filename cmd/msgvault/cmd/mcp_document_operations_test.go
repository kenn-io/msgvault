package cmd

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
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
	job      string
	manual   bool
}

func (s *documentScheduler) IsJobScheduled(name string) bool {
	return name == api.DocbankAttachmentMirrorJob
}
func (s *documentScheduler) StartJob(name string, manual bool) (scheduler.JobDisposition, error) {
	s.starts++
	s.job, s.manual = name, manual
	return "", s.startErr
}
func (s *documentScheduler) JobStatus() []api.JobStatus {
	return []api.JobStatus{{Name: api.DocbankAttachmentMirrorJob, LastError: "secret diagnostic /private/path"}}
}

func TestMCPDocumentMirrorNativeRetryAndProjection(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	f := storetest.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Integrations.Docbank.Enabled = true
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	cfg.Integrations.Docbank.DocumentProfile = "documents"
	hash := strings.Repeat("a", 64)
	requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), f.CreateMessage("mirror"), store.AttachmentWrite{ContentHash: hash, Size: 20, StoragePath: "aa/" + hash, State: "stored"}))
	requirements.NoError(f.Store.RecordDocbankAttachment(t.Context(), "destination", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{}, "secret reason"))
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
	call := func(name string) *sdkmcp.CallToolResult {
		t.Helper()
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		requirements.NoError(err)
		return result
	}
	status := loopToolOutput[map[string]any](t, call("get_docbank_attachment_status"))
	assertions.Equal("running", status["state"])
	assertions.Equal(true, status["has_last_error"])
	counts, ok := status["counts"].(map[string]any)
	requirements.True(ok)
	assertions.InDelta(1, counts["failed"], 0)
	assertions.NotContains(counts, "reasons")
	assertions.NotContains(status, "folder")
	assertions.NotContains(status, "last_error")
	processing, ok := status["processing"].(map[string]any)
	requirements.True(ok)
	assertions.NotContains(processing, "outcomes")
	assertions.True(call("retry_docbank_attachment_mirror").IsError)
	assertions.Zero(sched.starts)
	approve = true
	receipt := loopToolOutput[generated.DocbankAttachmentBackfillResponse](t, call("retry_docbank_attachment_mirror"))
	assertions.Equal("scheduled", receipt.Status)
	assertions.Contains(disclosure, "consent")
	assertions.Contains(disclosure, "upload")
	assertions.Equal(1, sched.starts)
	assertions.Equal(api.DocbankAttachmentMirrorJob, sched.job)
	assertions.False(sched.manual)
	due, err := f.Store.SendingDocbankAttachments(t.Context(), "destination")
	requirements.NoError(err)
	assertions.Len(due, 1)
	cfg.Integrations.Docbank.AttachmentUploadConsent = false
	off := loopToolOutput[map[string]any](t, call("get_docbank_attachment_status"))
	assertions.Equal("consent_off", off["state"])
	assertions.NotContains(off, "counts")
	assertions.NotContains(off, "processing")
	failure := loopToolFailure(t, call("retry_docbank_attachment_mirror"))
	assertions.Equal("conflict", failure["error"])
	assertions.Equal(false, failure["operation_may_have_completed"])
	cfg.Integrations.Docbank.AttachmentUploadConsent = true
	requirements.NoError(f.Store.RecordDocbankAttachment(t.Context(), "destination", store.DocbankAttachmentDelivery{ContentHash: hash}, store.DocbankAttachmentRef{}, "forbidden"))
	sched.startErr = errors.New("private scheduling diagnostic")
	failure = loopToolFailure(t, call("retry_docbank_attachment_mirror"))
	assertions.Equal("operation_refused", failure["error"])
	assertions.Equal(true, failure["operation_may_have_completed"])
	due, err = f.Store.SendingDocbankAttachments(t.Context(), "destination")
	requirements.NoError(err)
	assertions.Len(due, 1)
}

func TestMCPDocumentDiscoveryIsIndependent(t *testing.T) {
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
