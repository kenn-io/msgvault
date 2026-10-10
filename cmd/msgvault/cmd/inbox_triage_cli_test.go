package cmd

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// Missing commands, implicit apply, dropping the signed envelope, or discarding
// replay receipts break this real CLI -> HTTP -> Service -> Gmail path.
func TestInboxCLITriageNativePreviewApplyAndReceiptReplay(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified", true: "unknown"}[unknown], func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			a, target, writes, lost := nativeInboxTriageFixture(t)
			source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
			server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "synthetic-owner-key"}}, Store: a, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
			defer server.Close()
			ctx := withStoreResolverConfig(t, &config.Config{HomeDir: t.TempDir(), Remote: config.RemoteConfig{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true}})
			call := func(input any, args ...string) ([]byte, error) {
				data, err := json.Marshal(input)
				require.NoError(t, err)
				cmd := newInboxCmd()
				// ExecuteContext wraps the production command tree this way so
				// runtime errors preserve JSON stdout and usage errors show help.
				ensureSilenceUsageWrapped(cmd)
				cmd.SetContext(ctx)
				cmd.SetArgs(args)
				cmd.SetIn(bytes.NewReader(data))
				var output bytes.Buffer
				cmd.SetOut(&output)
				cmd.SetErr(io.Discard)
				err = cmd.Execute()
				return output.Bytes(), err
			}
			input := inboxcontrol.TriageInput{Source: source, Items: []inboxcontrol.TriageItemInput{{Target: target, Categories: []string{"todo"}, EvidenceMessageIDs: []int64{target.ItemID}}}}
			data, err := call(input, "triage", "preview", "--request", "-")
			requirements.NoError(err)
			var proposal inboxcontrol.TriageProposal
			requirements.NoError(json.Unmarshal(data, &proposal))
			requirements.Len(proposal.Items, 1)
			assertions.NotEmpty(proposal.PreviewToken)
			assertions.NotEmpty(proposal.Items[0].Request.IdempotencyKey)
			assertions.Zero(writes.Load())
			_, err = call(proposal, "triage", "apply", "--request", "-")
			requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
			assertions.Zero(writes.Load())
			lost.Store(unknown)
			data, err = call(proposal, "triage", "apply", "--request", "-", "--apply")
			if unknown {
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
			} else {
				requirements.NoError(err)
			}
			var results []inboxcontrol.Result
			requirements.NoError(json.Unmarshal(data, &results))
			requirements.Len(results, 1)
			requirements.NotNil(results[0].Receipt)
			if unknown {
				assertions.Equal(inboxcontrol.StatusUnknown, results[0].Receipt.Status)
				assertions.Equal(int64(1), writes.Load())
				data, err = call(proposal, "triage", "apply", "--request", "-", "--apply")
				requirements.ErrorIs(err, inboxcontrol.ErrOutcomeUnknown)
				var replay []inboxcontrol.Result
				requirements.NoError(json.Unmarshal(data, &replay))
				requirements.Len(replay, 1)
				requirements.NotNil(replay[0].Receipt)
				assertions.Equal(results[0].Receipt.ID, replay[0].Receipt.ID)
				assertions.Equal(int64(1), writes.Load())
				return
			}
			assertions.Equal(inboxcontrol.StatusVerified, results[0].Receipt.Status)
			assertions.True(*results[0].After.Inbox)
			assertions.False(*results[0].After.Read)
			assertions.Contains(results[0].After.Tags, "Unrelated")
			assertions.Equal(int64(1), writes.Load())
			data, err = call(proposal, "triage", "apply", "--request", "-", "--apply")
			requirements.NoError(err)
			var replay []inboxcontrol.Result
			requirements.NoError(json.Unmarshal(data, &replay))
			requirements.Len(replay, 1)
			requirements.NotNil(replay[0].Receipt)
			assertions.Equal(results[0].Receipt.ID, replay[0].Receipt.ID)
			assertions.Equal(int64(1), writes.Load())
		})
	}
}
