package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type inboxCLIStore struct {
	*storeAPIAdapter

	requests chan inboxcontrol.Request
	result   *inboxcontrol.Result
	err      error
}

func (s *inboxCLIStore) ControlInbox(ctx context.Context, r inboxcontrol.Request, p inboxcontrol.Principal, authorize func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error, _ func(context.Context) (func(), error)) (*inboxcontrol.Result, error) {
	if err := authorize(ctx, p, r); err != nil {
		return nil, err
	}
	s.requests <- r
	return s.result, s.err
}

var _ api.InboxController = (*inboxCLIStore)(nil)

func inboxCLIFixture(t *testing.T, result *inboxcontrol.Result, callErr error) (context.Context, *inboxCLIStore) {
	t.Helper()
	fixture := newDraftReplyFixture(t)
	backend := &inboxCLIStore{storeAPIAdapter: fixture.grantedAdapter(), requests: make(chan inboxcontrol.Request, 8), result: result, err: callErr}
	router := api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{HomeDir: t.TempDir(), Server: config.ServerConfig{APIKey: "owner-test-key"}}, Store: backend, Logger: slog.New(slog.DiscardHandler)}).Router()
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}}), backend
}
func TestInboxCLIArchiveDefaultsPreviewAndPreservesUnknownReceipt(t *testing.T) {
	target := inboxcontrol.Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com", Scope: inboxcontrol.ScopeMessage, ItemID: 2, ProviderID: "m2"}
	state := inboxcontrol.State{Target: target, ObservedAt: time.Now().UTC()}
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "apply unknown"}[apply], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			result := &inboxcontrol.Result{Before: &state, Projected: &state, PreviewToken: "signed"}
			var callErr error
			request := inboxcontrol.Request{Target: &target}
			args := []string{"archive", "--request", "-"}
			if apply {
				args = append(args, "--apply")
				request.Expected = &state
				request.PreviewToken = "signed"
				request.IdempotencyKey = "once"
				result = &inboxcontrol.Result{Receipt: &inboxcontrol.Receipt{ID: "r1", Status: inboxcontrol.StatusUnknown}}
				callErr = inboxcontrol.ErrOutcomeUnknown
			}
			ctx, backend := inboxCLIFixture(t, result, callErr)
			input, err := json.Marshal(request)
			requirements.NoError(err)
			command := newInboxCmd()
			command.SetContext(ctx)
			command.SetArgs(args)
			command.SetIn(bytes.NewReader(input))
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			err = command.Execute()
			if apply {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
				assertions.Contains(output.String(), "r1")
				assertions.Contains(output.String(), "unknown")
			} else {
				requirements.NoError(err)
				assertions.Contains(output.String(), "signed")
			}
			requirements.Len(backend.requests, 1)
			received := <-backend.requests
			assertions.Equal(inboxcontrol.OpArchive, received.Operation)
			assertions.Equal(!apply, received.DryRun)
			assertions.Equal(target, *received.Target)
			assertions.Equal(request.IdempotencyKey, received.IdempotencyKey)
		})
	}
}
func TestInboxCLIRejectsInvalidInputBeforeHTTPControl(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		args        []string
	}{
		{"unknown member", `{"surprise":true}`, []string{"get-state", "--request", "-"}},
		{"operation override", `{"operation":"move"}`, []string{"archive", "--request", "-"}},
		{"oversize", string(bytes.Repeat([]byte(" "), (1<<20)+1)), []string{"get-state", "--request", "-"}},
		{"unsigned apply", `{"target":{"source_id":1,"source_type":"gmail","source_identifier":"reader@example.com","account_id":"reader@example.com","scope":"message","item_id":2,"provider_id":"m2"}}`, []string{"archive", "--request", "-", "--apply"}},
		{"apply read", `{"receipt_id":"r1"}`, []string{"receipt-get", "--request", "-", "--apply"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, backend := inboxCLIFixture(t, nil, nil)
			command := newInboxCmd()
			command.SetContext(ctx)
			command.SetArgs(tc.args)
			command.SetIn(bytes.NewBufferString(tc.input))
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			require.Error(t, command.Execute())
			assert.Empty(t, backend.requests)
		})
	}
}

func TestInboxCLIFileSourceAndReceiptRequests(t *testing.T) {
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "gmail", SourceIdentifier: "reader@example.com", AccountID: "reader@example.com"}
	for _, tc := range []struct {
		name    string
		request inboxcontrol.Request
		result  *inboxcontrol.Result
	}{
		{"get-capabilities", inboxcontrol.Request{Source: &source}, &inboxcontrol.Result{Capabilities: &inboxcontrol.Capabilities{Source: source, LocationModel: "labels", Operations: []inboxcontrol.Capability{{Operation: inboxcontrol.OpGetCapabilities, Status: inboxcontrol.CapabilitySupported}}}}},
		{"receipt-get", inboxcontrol.Request{ReceiptID: "r1"}, &inboxcontrol.Result{Receipt: &inboxcontrol.Receipt{ID: "r1", Status: inboxcontrol.StatusVerified}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			ctx, backend := inboxCLIFixture(t, tc.result, nil)
			input, err := json.Marshal(tc.request)
			requirements.NoError(err)
			path := filepath.Join(t.TempDir(), "request.json")
			requirements.NoError(os.WriteFile(path, input, 0o600))
			command := newInboxCmd()
			command.SetContext(ctx)
			command.SetArgs([]string{tc.name, "--request", path})
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(io.Discard)
			requirements.NoError(command.Execute())
			requirements.Len(backend.requests, 1)
			got := <-backend.requests
			assertions.Equal(inboxcontrol.Operation(tc.name), got.Operation)
			assertions.Equal(tc.request.Source, got.Source)
			assertions.Equal(tc.request.ReceiptID, got.ReceiptID)
			assertions.False(got.DryRun)
			var result inboxcontrol.Result
			requirements.NoError(json.Unmarshal(output.Bytes(), &result))
			assertions.Equal(tc.result, &result)
		})
	}
}
func TestInboxCLIAllActionsAdmitDelegatedMode(t *testing.T) {
	parent := newInboxCmd()
	require.NotEmpty(t, parent.Commands())
	for _, command := range parent.Commands() {
		assert.True(t, agentDelegatedCapable(command), command.Name())
	}
	assert.False(t, agentDelegatedCapable(&cobra.Command{Use: "archive"}), "unrelated commands do not inherit inbox admission")
}
