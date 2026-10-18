package sync

import (
	"context"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available beside local helpers.
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

type delayedGmailLabelInventoryAPI struct {
	*gmail.MockAPI
	initialLabels []*gmail.Label
	currentLabels []*gmail.Label
	labelCalls    int
}

func (api *delayedGmailLabelInventoryAPI) NativeMessageReadPermission() (bool, bool) {
	return true, true
}

func (api *delayedGmailLabelInventoryAPI) ListLabels(context.Context) ([]*gmail.Label, error) {
	api.labelCalls++
	labels := api.initialLabels
	if api.labelCalls > 1 {
		labels = api.currentLabels
	}
	result := make([]*gmail.Label, len(labels))
	for i, label := range labels {
		copy := *label
		result[i] = &copy
	}
	return result, nil
}

func TestMCPProviderGmailIncrementalArchivesAndCapturesMessageWithNewLabel(t *testing.T) {
	for _, capture := range []bool{false, true} {
		name := "archive-only"
		if capture {
			name = "events-capture"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			env := newTestEnv(t)
			source := env.CreateSourceWithHistory(t, "1000")
			oldLabels := []*gmail.Label{{ID: "INBOX", Name: "INBOX", Type: "system"}}
			newLabels := append(append([]*gmail.Label(nil), oldLabels...), &gmail.Label{ID: "Label_Race", Name: "Synthetic new label", Type: "user"})
			client := &delayedGmailLabelInventoryAPI{
				MockAPI:       env.Mock,
				initialLabels: oldLabels,
				currentLabels: newLabels,
			}
			env.Syncer = New(client, env.Store, nil)
			if capture {
				_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
					Enabled: true, Principal: "synthetic-owner",
					Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
				})
				require.NoError(err)
			}
			env.Mock.AddMessage("live-label-race", testemail.NewMessage().
				From("sender@example.test").
				To(testEmail).
				Body("Synthetic message carrying a newly created label").
				Bytes(), []string{"INBOX", "Label_Race"})
			env.SetHistory(1001, historyAdded("live-label-race"))

			_, err := env.Syncer.Incremental(env.Context, source)
			require.NoError(err, "incremental sync must archive a message whose label inventory changed after the first ListLabels call")
			assertMessageCount(t, env.Store, 1)
			assertMessageHasLabel(t, env.Store, "live-label-race", "Label_Race")

			if capture {
				assert.GreaterOrEqual(client.labelCalls, 2, "captured label membership must refresh the admitted scope inventory before persistence")
				assert.Equal(1, providerEventCount(t, env), "the captured live Gmail message must emit one event")
			} else {
				assert.Zero(providerEventCount(t, env), "archive-only sync must not produce MCP Events")
			}
		})
	}
}

func TestMCPProviderGmailIncrementalFailsWhenRefreshedInventoryOmitsMessageLabel(t *testing.T) {
	for _, capture := range []bool{false, true} {
		name := "archive-only"
		if capture {
			name = "events-capture"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			env := newTestEnv(t)
			source := env.CreateSourceWithHistory(t, "1000")
			labels := []*gmail.Label{{ID: "INBOX", Name: "INBOX", Type: "system"}}
			client := &delayedGmailLabelInventoryAPI{
				MockAPI:       env.Mock,
				initialLabels: labels,
				currentLabels: labels,
			}
			env.Syncer = New(client, env.Store, nil)
			if capture {
				_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
					Enabled: true, Principal: "synthetic-owner",
					Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
				})
				require.NoError(err)
			}
			env.Mock.AddMessage("live-label-race", testemail.NewMessage().
				From("sender@example.test").
				To(testEmail).
				Body("Synthetic message carrying a label missing from the refreshed inventory").
				Bytes(), []string{"INBOX", "Label_Race"})
			env.SetHistory(1001, historyAdded("live-label-race"))

			_, err := env.Syncer.Incremental(env.Context, source)
			require.ErrorContains(err, "native Gmail label unavailable", "an unconfirmed message label must fail the run instead of advancing history")
			assert.GreaterOrEqual(client.labelCalls, 2, "sync must refresh the provider inventory before deciding the label is unavailable")
			assertMessageCount(t, env.Store, 0)
			assert.Zero(providerEventCount(t, env), "an unconfirmed label must not create an event")
			refreshed, err := env.Store.GetSourceByID(source.ID)
			require.NoError(err)
			assert.Equal("1000", refreshed.SyncCursor.String, "history must remain retryable when refreshed labels omit the live message label")
		})
	}
}
