package cmd

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxCLIClosedStdoutRecoversWithoutReplay(t *testing.T) {
	if endpoint := os.Getenv("MSGVAULT_TEST_TRIAGE_STDOUT_URL"); endpoint != "" {
		command := newInboxCmd()
		ensureSilenceUsageWrapped(command)
		command.SetContext(withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: endpoint, APIKey: "synthetic-owner-key", AllowInsecure: true}}))
		args := []string{"triage", "apply", "--request", "-", "--apply"}
		if os.Getenv("MSGVAULT_TEST_INBOX_STDOUT_MODE") == "tags" {
			args = []string{"tags", "--request", "-", "--apply"}
		}
		command.SetArgs(args)
		command.SetOut(os.Stdout)
		command.SetErr(io.Discard)
		err := command.Execute()
		if errors.Is(err, inboxcontrol.ErrOutcomeUnknown) {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(23)
		}
		_, _ = fmt.Fprintln(os.Stderr, "expected unknown outcome:", err)
		os.Exit(24)
	}
	if runtime.GOOS == "windows" {
		t.Skip("SIGPIPE process behavior requires Unix")
	}
	for _, mode := range []string{"triage", "tags"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			a, target, writes, _ := nativeInboxTriageFixture(t)
			server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, Store: a, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true})
			requirements.NoError(err)
			t.Cleanup(func() { _ = client.Close() })
			source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
			proposal, err := client.PreviewInboxTriage(t.Context(), inboxcontrol.TriageInput{Source: source, Items: []inboxcontrol.TriageItemInput{{Target: target, Categories: []string{"todo"}, EvidenceMessageIDs: []int64{target.ItemID}}}})
			requirements.NoError(err)
			var input any = proposal
			direct := proposal.Items[0].Request
			if mode == "tags" {
				direct.DryRun = true
				direct.Expected = nil
				direct.IdempotencyKey = ""
				preview, err := client.ControlInbox(t.Context(), direct)
				requirements.NoError(err)
				direct.DryRun = false
				direct.Expected = preview.Before
				direct.PreviewToken = preview.PreviewToken
				direct.IdempotencyKey = "synthetic-inbox-closed-stdout"
				input = direct
			}
			data, err := json.Marshal(input)
			requirements.NoError(err)
			reader, writer, err := os.Pipe()
			requirements.NoError(err)
			requirements.NoError(reader.Close())
			t.Cleanup(func() { _ = writer.Close() })
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestInboxCLIClosedStdoutRecoversWithoutReplay$") //nolint:gosec // Re-executes this test binary with a fixed test selector.
			child.Env = append(os.Environ(), "MSGVAULT_TEST_TRIAGE_STDOUT_URL="+server.URL, "MSGVAULT_TEST_INBOX_STDOUT_MODE="+mode)
			child.Stdin = bytes.NewReader(data)
			child.Stdout = writer
			var stderr bytes.Buffer
			child.Stderr = &stderr
			err = child.Run()
			var exit *exec.ExitError
			requirements.ErrorAs(err, &exit)
			assertions.Equal(23, exit.ExitCode(), stderr.String())
			assertions.Contains(stderr.String(), "outcome unknown")
			assertions.Equal(int64(1), writes.Load())
			var results []inboxcontrol.Result
			if mode == "tags" {
				result, recoverErr := client.ControlInbox(t.Context(), direct)
				requirements.NoError(recoverErr)
				requirements.NotNil(result)
				results = []inboxcontrol.Result{*result}
				err = nil
			} else {
				results, err = client.ApplyInboxTriage(t.Context(), *proposal)
			}
			requirements.NoError(err)
			requirements.Len(results, 1)
			requirements.NotNil(results[0].Receipt)
			assertions.Equal(inboxcontrol.StatusVerified, results[0].Receipt.Status)
			assertions.Equal(int64(1), writes.Load(), "receipt recovery must not replay a native write")
		})
	}
}
