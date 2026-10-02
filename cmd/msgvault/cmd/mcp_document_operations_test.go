package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document/mistral/mistraltest"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Execute the real production Cobra handlers over the genuine daemon HTTP
// stream; only the external extractor and attachment bytes are synthetic.
type inProcessDocumentDaemonStore struct {
	*storeAPIAdapter

	state    *invocation
	deps     documentsCommandDeps
	requests []api.CLIRunRequest
}

func (s *inProcessDocumentDaemonStore) RunCLICommand(ctx context.Context, req api.CLIRunRequest, emit func(api.CLIRunEvent) error) error {
	s.requests = append(s.requests, req)
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(newDocumentsCmd(s.deps))
	root.SetArgs(req.Args)
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	err := root.ExecuteContext(withInvocation(ctx, s.state))
	for _, event := range []api.CLIRunEvent{{Type: cliStreamStdout, Data: stdout.String()}, {Type: cliStreamStderr, Data: stderr.String()}} {
		if event.Data != "" {
			if emitErr := emit(event); emitErr != nil {
				return emitErr
			}
		}
	}
	if err != nil {
		return fmt.Errorf("execute production document command: %w", err)
	}
	return nil
}

func TestMCPDocumentLocalManifestBoundaryAndDurableStatus(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	ctx, manifest, _, st := documentJSONFixture(t)
	_, err := resolveMCPDocumentManifest(manifest, HTTPStoreInfo{Kind: HTTPStoreConfiguredRemote}, false)
	requirements.Error(err)
	_, err = resolveMCPDocumentManifest(manifest, HTTPStoreInfo{Kind: HTTPStoreAgentDelegated}, true)
	requirements.Error(err)
	path, err := resolveMCPDocumentManifest(manifest, HTTPStoreInfo{Kind: HTTPStoreLocalDaemon}, false)
	requirements.NoError(err)
	assertions.Equal(filepath.Clean(manifest), path)
	profile, err := configuredDocumentProfileOnly(manifest, invocationFromContext(ctx))
	requirements.NoError(err)
	_, err = st.EnsureDocumentExtractionProfile(ctx, profile)
	requirements.NoError(err)
	requirements.NoError(st.RecordDocumentProviderConsent(ctx, store.DocumentProviderConsent{ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint, RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture}))
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: config.NewDefaultConfig(), Store: &storeAPIAdapter{store: st, mcpCommands: registeredMCPCommandDescriptors()}, Logger: slog.New(slog.DiscardHandler)}))
	assertions.Contains(backend.capabilities(), "get_document_index_status")
	assertions.NotContains(backend.capabilities(), "get_document_processing_policy")
	assertions.NotContains(backend.capabilities(), "build_document_index")
	result, err := backend.ExecuteOperation(t.Context(), "get_document_index_status", nil)
	requirements.NoError(err)
	status := operationOutput[mcpserver.DocumentIndexStatus](t, result)
	assertions.True(status.Status.ExactConsent)
	backend.useDocumentManifest(path)
	assertions.Contains(backend.capabilities(), "get_document_processing_policy")
	assertions.Contains(backend.capabilities(), "build_document_index")
	for _, key := range []string{"capabilities", "path", "cwd", "env"} {
		result, err := backend.ExecuteOperation(t.Context(), "build_document_index", map[string]any{"fingerprint": strings.Repeat("a", 64), key: "untrusted"})
		requirements.NoError(err)
		assertions.True(result.IsError)
	}
}

func TestMCPDocumentApprovalPartialRetryAndRebuildUseProductionHandlers(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	markDaemonCLISubprocessForTest(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Attachments.Documents.Enabled = true
	cfg.Attachments.Documents.RetentionPosture = documentindex.RetentionStandard
	cfg.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut
	fixture := storetest.New(t)
	contents := map[string][]byte{}
	for _, text := range []string{"synthetic first", "synthetic second"} {
		content := mistraltest.MinimalPDF(text)
		hash := sha256.Sum256(content)
		digest := hex.EncodeToString(hash[:])
		contents[digest] = content
		requirements.NoError(fixture.Store.UpsertAttachmentRecord(t.Context(), fixture.CreateMessage(text), store.AttachmentWrite{Filename: "synthetic.pdf", MIMEType: "application/pdf", Size: int64(len(content)), StoragePath: digest[:2] + "/" + digest, ContentHash: digest, Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1"}))
	}
	processor := &commandBuildProcessor{firstErr: errors.New("synthetic-extractor-private-canary")}
	deps := documentsCommandDeps{openStore: func(context.Context) (*store.Store, func(), error) { return fixture.Store, func() {}, nil }, newMistralProcessor: func(*documentindex.DocumentsConfig) (documentindex.MistralProcessor, error) { return processor, nil }, openAttachments: func(context.Context, *store.Store) (documentindex.DocumentAttachmentOpener, func() error, error) {
		return commandAttachmentMapOpener{contents: contents}, func() error { return nil }, nil
	}}
	daemon := &inProcessDocumentDaemonStore{storeAPIAdapter: &storeAPIAdapter{store: fixture.Store, mcpCommands: registeredMCPCommandDescriptors()}, state: invocationFromContext(testInvocationContext(t.Context(), cfg, invocationOptions{})), deps: deps}
	backend := sourceOperationFixture(t, api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: daemon, Logger: slog.New(slog.DiscardHandler)}))
	backend.useDocumentManifest(writeCommandCapabilityManifest(t, cfg.Attachments.Documents.MaxPagesPerDocument))
	policyResult, err := backend.ExecuteOperation(t.Context(), "get_document_processing_policy", nil)
	requirements.NoError(err)
	policy := operationOutput[mcpserver.DocumentPolicy](t, policyResult)
	assertions.False(policy.ExactConsent)
	action := "decline"
	changePolicyDuringApproval := false
	session := operationMCPSession(t, backend, []mcpserver.OperationFamily{mcpserver.OperationFamilyDocuments}, &sdkmcp.ClientOptions{ElicitationHandler: func(_ context.Context, request *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
		assertions.Contains(request.Params.Message, policy.Fingerprint)
		if changePolicyDuringApproval {
			cfg.Attachments.Documents.MaxFileBytes--
		}
		return &sdkmcp.ElicitResult{Action: action, Content: map[string]any{"confirm": true}}, nil
	}})
	args := map[string]any{"fingerprint": policy.Fingerprint}
	called, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "consent_document_processing", Arguments: args})
	requirements.NoError(err)
	assertions.True(called.IsError)
	status, err := fixture.Store.GetDocumentIndexStatus(t.Context(), policy.ProfileID)
	requirements.NoError(err)
	assertions.False(status.ProfileExists)
	action = "accept"
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "consent_document_processing", Arguments: args})
	requirements.NoError(err)
	requirements.False(called.IsError)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "build_document_index", Arguments: args})
	requirements.NoError(err)
	requirements.True(called.IsError)
	data, err := json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	assertions.NotContains(string(data), "synthetic-extractor-private-canary")
	var partial mcpserver.DocumentBuild
	requirements.NoError(json.Unmarshal(data, &partial))
	assertions.Equal(2, partial.Attempted)
	assertions.Equal(1, partial.Succeeded)
	assertions.Equal(1, partial.Failed)
	requirements.Len(partial.Failures, 1)
	assertions.Positive(partial.RunID)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "retry_document_extraction", Arguments: map[string]any{"fingerprint": policy.Fingerprint, "canonical_blob_hash": partial.Failures[0].CanonicalBlobHash}})
	requirements.NoError(err)
	requirements.False(called.IsError)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "resume_document_index", Arguments: args})
	requirements.NoError(err)
	requirements.False(called.IsError)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "build_document_index", Arguments: map[string]any{"fingerprint": policy.Fingerprint, "limit": 1, "full_rebuild": true}})
	requirements.NoError(err)
	requirements.False(called.IsError)
	data, err = json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var rebuild mcpserver.DocumentBuild
	requirements.NoError(json.Unmarshal(data, &rebuild))
	assertions.Equal("pending", rebuild.State)
	assertions.NotEmpty(rebuild.RebuildID)
	assertions.Equal(int64(1), rebuild.Remaining)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "resume_document_index", Arguments: map[string]any{"fingerprint": policy.Fingerprint, "limit": 1}})
	requirements.NoError(err)
	requirements.False(called.IsError)
	data, err = json.Marshal(called.StructuredContent)
	requirements.NoError(err)
	var completed mcpserver.DocumentBuild
	requirements.NoError(json.Unmarshal(data, &completed))
	assertions.Equal(rebuild.RebuildID, completed.RebuildID)
	assertions.True(completed.Completed)
	assertions.Zero(completed.Remaining)
	completedCalls := processor.calls
	changePolicyDuringApproval = true
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "build_document_index", Arguments: args})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Equal(completedCalls, processor.calls)
	changePolicyDuringApproval = false
	cfg.Attachments.Documents.MaxFileBytes++
	_, err = fixture.Store.RetireDocumentExtractionProfile(t.Context(), policy.ProfileID)
	requirements.NoError(err)
	policyResult, err = backend.ExecuteOperation(t.Context(), "get_document_processing_policy", nil)
	requirements.NoError(err)
	retired := operationOutput[mcpserver.DocumentPolicy](t, policyResult)
	assertions.True(retired.ExactConsent)
	assertions.False(retired.ProfileEnabled)
	called, err = session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "build_document_index", Arguments: args})
	requirements.NoError(err)
	assertions.True(called.IsError)
	assertions.Equal(completedCalls, processor.calls)
	for _, request := range daemon.requests {
		assertions.Empty(request.Env)
		assertions.Empty(request.Cwd)
		assertions.False(request.GrantDecided)
	}
}
