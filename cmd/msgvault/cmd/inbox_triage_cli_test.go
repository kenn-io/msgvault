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
			require.NoError(t, err)
			var proposal inboxcontrol.TriageProposal
			require.NoError(t, json.Unmarshal(data, &proposal))
			require.Len(t, proposal.Items, 1)
			assert.NotEmpty(t, proposal.PreviewToken)
			assert.NotEmpty(t, proposal.Items[0].Request.IdempotencyKey)
			assert.Zero(t, writes.Load())
			_, err = call(proposal, "triage", "apply", "--request", "-")
			assert.ErrorIs(t, err, inboxcontrol.ErrInvalid)
			assert.Zero(t, writes.Load())
			lost.Store(unknown)
			data, err = call(proposal, "triage", "apply", "--request", "-", "--apply")
			if unknown {
				require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
			} else {
				require.NoError(t, err)
			}
			var results []inboxcontrol.Result
			require.NoError(t, json.Unmarshal(data, &results))
			require.Len(t, results, 1)
			require.NotNil(t, results[0].Receipt)
			if unknown {
				assert.Equal(t, inboxcontrol.StatusUnknown, results[0].Receipt.Status)
				assert.Equal(t, int64(1), writes.Load())
				data, err = call(proposal, "triage", "apply", "--request", "-", "--apply")
				require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
				var replay []inboxcontrol.Result
				require.NoError(t, json.Unmarshal(data, &replay))
				require.Len(t, replay, 1)
				require.NotNil(t, replay[0].Receipt)
				assert.Equal(t, results[0].Receipt.ID, replay[0].Receipt.ID)
				assert.Equal(t, int64(1), writes.Load())
				return
			}
			assert.Equal(t, inboxcontrol.StatusVerified, results[0].Receipt.Status)
			assert.True(t, *results[0].After.Inbox)
			assert.False(t, *results[0].After.Read)
			assert.Contains(t, results[0].After.Tags, "Unrelated")
			assert.Equal(t, int64(1), writes.Load())
			data, err = call(proposal, "triage", "apply", "--request", "-", "--apply")
			require.NoError(t, err)
			var replay []inboxcontrol.Result
			require.NoError(t, json.Unmarshal(data, &replay))
			require.Len(t, replay, 1)
			require.NotNil(t, replay[0].Receipt)
			assert.Equal(t, results[0].Receipt.ID, replay[0].Receipt.ID)
			assert.Equal(t, int64(1), writes.Load())
		})
	}
}
