package cmd

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestIdentityCLIRequiresExplicitApplyAndReadsNativeReceipt(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, f.grantedAdapter(), nil)
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
	first, err := f.store.EnsureParticipant("sender@example.com", "Sender", "example.com")
	requirements.NoError(err)
	second, err := f.store.EnsureParticipant(testutil.IMAPTestUsername, "", "example.com")
	requirements.NoError(err)
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}}
	run := func(args []string, body any) ([]byte, error) {
		command := newIdentityOperationsCmd()
		command.SetContext(ctx)
		command.SetArgs(args)
		data, marshalErr := json.Marshal(body)
		requirements.NoError(marshalErr)
		command.SetIn(bytes.NewReader(data))
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(io.Discard)
		callErr := command.Execute()
		return output.Bytes(), callErr
	}
	output, err := run([]string{"graph-link", "--request", "-"}, intent)
	requirements.NoError(err)
	var preview struct {
		Snapshot store.IdentitySnapshot `json:"snapshot"`
		Token    string                 `json:"preview_token"`
	}
	requirements.NoError(json.Unmarshal(output, &preview))
	requirements.NotEmpty(preview.Token)
	state, err := f.store.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Empty(state.Links, "default command is read-only")
	apply := map[string]any{"operation": intent.Operation, "target": intent.Target, "expected_fingerprint": preview.Snapshot.Fingerprint, "preview_token": preview.Token, "idempotency_key": "synthetic-cli-link"}
	output, err = run([]string{"graph-link", "--request", "-", "--apply"}, apply)
	requirements.NoError(err)
	var receipt store.IdentityReceipt
	requirements.NoError(json.Unmarshal(output, &receipt))
	assertions.True(receipt.Changed)
	state, err = f.store.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Len(state.Links, 1)
	output, err = run([]string{"receipt", "--idempotency-key", "synthetic-cli-link"}, nil)
	requirements.NoError(err)
	var readback store.IdentityReceipt
	requirements.NoError(json.Unmarshal(output, &readback))
	assertions.Equal(receipt, readback)
	// An exact committed retry remains a receipt lookup even after preview loss.
	apply["preview_token"] = "expired-preview"
	output, err = run([]string{"graph-link", "--request", "-", "--apply"}, apply)
	requirements.NoError(err)
	requirements.NoError(json.Unmarshal(output, &readback))
	assertions.Equal(receipt, readback)
}

func TestIdentityCLIDelegatedStartupUsesNativeReadGrant(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, f.grantedAdapter(), nil)
	ctx := mcpDraftAgentContext(t, server, f.source.ID, []string{"identity.read"})
	first, err := f.store.EnsureParticipant("sender@example.com", "Sender", "example.com")
	requirements.NoError(err)
	second, err := f.store.EnsureParticipant(testutil.IMAPTestUsername, "", "example.com")
	requirements.NoError(err)
	root := newRootCommand()
	group := &cobra.Command{Use: "identity"}
	group.AddCommand(newIdentityOperationsCmd())
	root.AddCommand(group)
	root.SetContext(ctx)
	root.SetArgs([]string{"--agent-url", server.URL, "--agent-token-file", optionsFromContext(ctx).agentTokenFile, "--agent-allow-insecure", "identity", "operations", "graph-link", "--request", "-"})
	data, err := json.Marshal(identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}})
	requirements.NoError(err)
	root.SetIn(bytes.NewReader(data))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(io.Discard)
	requirements.NoError(root.Execute())
	assertions.Contains(output.String(), "preview_token")
	state, err := f.store.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second})
	requirements.NoError(err)
	assertions.Empty(state.Links)
}

func TestIdentityCLIRejectsInvalidInputBeforeNativeCall(t *testing.T) {
	f := newDraftReplyFixture(t)
	var calls atomic.Int64
	server := mcpDraftTestDaemon(t, f.grantedAdapter(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/v1/identity/operations/") {
				calls.Add(1)
			}
			next.ServeHTTP(w, r)
		})
	})
	ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}})
	first, err := f.store.EnsureParticipant("sender@example.com", "Sender", "example.com")
	require.NoError(t, err)
	second, err := f.store.EnsureParticipant(testutil.IMAPTestUsername, "", "example.com")
	require.NoError(t, err)
	valid, err := json.Marshal(identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, input string
		apply       bool
	}{
		{"unknown member", `{"surprise":true}`, false},
		{"operation override", `{"operation":"graph-unlink","target":{"participant_id":1,"other_participant_id":2}}`, false},
		{"unsafe integer", `{"target":{"participant_id":9007199254740992,"other_participant_id":2}}`, false},
		{"duplicate field", `{"target":{"participant_id":1,"participant_id":2,"other_participant_id":3}}`, false},
		{"null field", strings.TrimSuffix(string(valid), "}") + `,"preview_token":null}`, false},
		{"oversize", string(bytes.Repeat([]byte(" "), 16*1024+1)), false},
		{"unsigned apply", string(valid), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			command := newIdentityOperationsCmd()
			command.SetContext(ctx)
			args := []string{"graph-link", "--request", "-"}
			if tc.apply {
				args = append(args, "--apply")
			}
			command.SetArgs(args)
			command.SetIn(strings.NewReader(tc.input))
			command.SetOut(io.Discard)
			command.SetErr(io.Discard)
			require.Error(t, command.Execute())
			assert.Equal(t, before, calls.Load())
		})
	}
}

func TestIdentityCLIOutputLossPreservesCommittedReceipt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, f.grantedAdapter(), nil)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	first, err := f.store.EnsureParticipant("sender@example.com", "Sender", "example.com")
	requirements.NoError(err)
	second, err := f.store.EnsureParticipant(testutil.IMAPTestUsername, "", "example.com")
	requirements.NoError(err)
	preview, err := client.PreviewIdentityOperation(t.Context(), identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}})
	requirements.NoError(err)
	data, err := json.Marshal(map[string]any{"target": map[string]any{"participant_id": first, "other_participant_id": second}, "expected_fingerprint": preview.Snapshot.Fingerprint, "preview_token": preview.PreviewToken, "idempotency_key": "synthetic-cli-output-loss"})
	requirements.NoError(err)
	command := newIdentityOperationsCmd()
	command.SetContext(withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true}}))
	command.SetArgs([]string{"graph-link", "--request", "-", "--apply"})
	command.SetIn(bytes.NewReader(data))
	command.SetOut(messageTagFailWriter{})
	command.SetErr(io.Discard)
	err = command.Execute()
	unknown, ok := errors.AsType[*daemonclient.IdentityOutcomeUnknownError](err)
	requirements.True(ok, "committed output loss requires receipt reconciliation: %v", err)
	assertions.Equal("synthetic-cli-output-loss", unknown.IdempotencyKey)
	requirements.ErrorIs(err, io.ErrClosedPipe)
	receipt, err := client.GetIdentityOperationReceipt(t.Context(), unknown.IdempotencyKey)
	requirements.NoError(err)
	assertions.True(receipt.Changed)
	state, err := f.store.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second})
	requirements.NoError(err)
	assertions.Len(state.Links, 1)
}

func TestIdentityCLIClosedStdoutPreservesCommittedReceipt(t *testing.T) {
	if endpoint := os.Getenv("MSGVAULT_TEST_IDENTITY_STDOUT_URL"); endpoint != "" {
		command := newIdentityOperationsCmd()
		ensureSilenceUsageWrapped(command)
		command.SetContext(withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: endpoint, APIKey: "owner-test-key", AllowInsecure: true}}))
		command.SetArgs([]string{"graph-link", "--request", "-", "--apply"})
		command.SetOut(os.Stdout)
		command.SetErr(io.Discard)
		err := command.Execute()
		if _, unknown := errors.AsType[*daemonclient.IdentityOutcomeUnknownError](err); unknown {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(23)
		}
		_, _ = fmt.Fprintln(os.Stderr, "expected unknown outcome:", err)
		os.Exit(24)
	}
	if runtime.GOOS == "windows" {
		t.Skip("SIGPIPE process behavior requires Unix")
	}
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, f.grantedAdapter(), nil)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	first, err := f.store.EnsureParticipant("sender@example.com", "Sender", "example.com")
	requirements.NoError(err)
	second, err := f.store.EnsureParticipant(testutil.IMAPTestUsername, "", "example.com")
	requirements.NoError(err)
	intent := identitycontrol.PreviewRequest{Operation: identitycontrol.OperationGraphLink, Target: identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second}}
	preview, err := client.PreviewIdentityOperation(t.Context(), intent)
	requirements.NoError(err)
	const key = "synthetic-cli-closed-stdout"
	data, err := json.Marshal(map[string]any{"target": intent.Target, "expected_fingerprint": preview.Snapshot.Fingerprint, "preview_token": preview.PreviewToken, "idempotency_key": key})
	requirements.NoError(err)
	reader, writer, err := os.Pipe()
	requirements.NoError(err)
	requirements.NoError(reader.Close())
	t.Cleanup(func() { _ = writer.Close() })
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestIdentityCLIClosedStdoutPreservesCommittedReceipt$") //nolint:gosec // Re-executes this test binary with a fixed test selector.
	child.Env = append(os.Environ(), "MSGVAULT_TEST_IDENTITY_STDOUT_URL="+server.URL)
	child.Stdin = bytes.NewReader(data)
	child.Stdout = writer
	var stderr bytes.Buffer
	child.Stderr = &stderr
	err = child.Run()
	exit, ok := errors.AsType[*exec.ExitError](err)
	requirements.True(ok, "expected receipt recovery exit: %v", err)
	assertions.Equal(23, exit.ExitCode(), stderr.String())
	assertions.Contains(stderr.String(), "outcome unknown")
	assertions.Contains(stderr.String(), key)
	assertions.Contains(stderr.String(), "receipt")
	receipt, err := client.GetIdentityOperationReceipt(t.Context(), key)
	requirements.NoError(err)
	assertions.True(receipt.Changed)
	state, err := f.store.IdentityOperationPreviewContext(t.Context(), intent.Operation, intent.Target)
	requirements.NoError(err)
	assertions.Len(state.Links, 1)
}
