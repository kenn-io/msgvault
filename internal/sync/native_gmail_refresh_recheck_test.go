package sync

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

type mutableGmailReadPermission struct {
	read  atomic.Bool
	known atomic.Bool
}

func newMutableGmailReadPermission(read bool) *mutableGmailReadPermission {
	permission := &mutableGmailReadPermission{}
	permission.read.Store(read)
	permission.known.Store(true)
	return permission
}

func (p *mutableGmailReadPermission) NativeMessageReadPermission() (bool, bool) {
	return p.read.Load(), p.known.Load()
}

type gmailPermissionReductionAfterRequestAPI struct {
	*gmail.MockAPI

	permission *mutableGmailReadPermission
	operation  string
	unknown    bool
}

func (c gmailPermissionReductionAfterRequestAPI) NativeMessageReadPermission() (bool, bool) {
	return c.permission.NativeMessageReadPermission()
}

func (c gmailPermissionReductionAfterRequestAPI) ListLabels(ctx context.Context) ([]*gmail.Label, error) {
	labels, err := c.MockAPI.ListLabels(ctx)
	if err == nil && c.operation == "labels" {
		c.permission.read.Store(false)
		if c.unknown {
			c.permission.known.Store(false)
		}
	}
	return labels, err
}

func (c gmailPermissionReductionAfterRequestAPI) ListHistory(ctx context.Context, start uint64, page string) (*gmail.HistoryResponse, error) {
	history, err := c.MockAPI.ListHistory(ctx, start, page)
	if err == nil && c.operation == "history" {
		c.permission.read.Store(false)
	}
	return history, err
}

func (c gmailPermissionReductionAfterRequestAPI) GetProfile(ctx context.Context) (*gmail.Profile, error) {
	if c.operation == "profile" {
		c.permission.read.Store(false)
		if c.unknown {
			c.permission.known.Store(false)
		}
	}
	return c.MockAPI.GetProfile(ctx)
}

type mixedDenialGmailBatchAPI struct {
	grantedGmailProvenanceAPI

	deniedID string
}

func (c mixedDenialGmailBatchAPI) GetMessagesRawBatchWithErrors(ctx context.Context, ids []string) ([]gmail.RawMessageBatchResult, error) {
	results, err := c.MockAPI.GetMessagesRawBatchWithErrors(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if id == c.deniedID {
			results[i].Message = nil
			results[i].Err = &gmail.StatusError{StatusCode: http.StatusForbidden}
		}
	}
	return results, nil
}

func TestIncrementalGmailMixedBatchDenialPreservesHistoryCursor(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	env := newTestEnv(t)
	enableProviderEvents(t, env, "gmail")
	source := env.CreateSourceWithHistory(t, "1000")
	const successfulID = "synthetic-batch-success"
	const deniedID = "synthetic-batch-denied"
	env.Mock.AddMessage(successfulID, testMIME(), []string{"INBOX"})
	env.Mock.AddMessage(deniedID, testMIME(), []string{"INBOX"})
	env.SetHistory(1001, historyAdded(successfulID), historyAdded(deniedID))
	env.Syncer = New(mixedDenialGmailBatchAPI{grantedGmailProvenanceAPI: grantedGmailProvenanceAPI{env.Mock}, deniedID: deniedID}, env.Store, nil)

	_, err := env.Syncer.Incremental(t.Context(), source)
	require.ErrorIs(err, errNativeSourcePermissionReduction)

	allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed)
	current, err := env.Store.GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(err)
	assert.Equal("1000", current.SyncCursor.String, "the denied mixed batch must leave history retryable")
	assert.Zero(providerEventCount(t, env))
	var archived int
	require.NoError(env.Store.DB().QueryRow(env.Store.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id IN (?, ?)`), source.ID, successfulID, deniedID).Scan(&archived))
	assert.Zero(archived, "successful batch items must not disappear into unreplayable ingest errors")
}

func TestIncrementalGmailSuccessfulRequestRefreshReductionRetiresEventsAuthority(t *testing.T) {
	for _, operation := range []string{"labels", "history"} {
		t.Run(operation, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			env := newTestEnv(t)
			source := env.CreateSourceWithHistory(t, "1000")
			env.SetHistory(1001)
			_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
				Enabled: true, Principal: "owner", Capabilities: []store.MCPEventCapability{{
					Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"},
				}},
			})
			require.NoError(err)

			pinned, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			sourcePermission := store.NativeSourcePermission{SourceID: source.ID, SourceReference: pinned.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}
			require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), sourcePermission))
			_, binding, err := env.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.source_message_archived", "source", source.ID, []string{"message"}, pinned.Reference)
			require.NoError(err)

			permission := newMutableGmailReadPermission(true)
			client := gmailPermissionReductionAfterRequestAPI{
				MockAPI: env.Mock, permission: permission, operation: operation,
			}
			worker := New(client, env.Store, nil)
			_, err = worker.Incremental(t.Context(), source)
			require.Error(err, "the sync must stop after persisting a successful-request permission reduction")

			allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "later refresh metadata must retire message-read authority")
			current, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.NotEqual(pinned.Reference, current.Reference, "the source lifetime must be retired")
			require.Error(env.Store.CheckMCPResourceBinding(t.Context(), binding), "retained event bindings must be revoked")

			currentSource, err := env.Store.GetSourceByIDContext(t.Context(), source.ID)
			require.NoError(err)
			assert.Equal("1000", currentSource.SyncCursor.String, "the history cursor must remain retryable")
		})
	}
}

func TestIncrementalGmailUnknownRefreshPermissionPausesEventsWithoutRetiringLifetime(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "1000")
	env.SetHistory(1001)
	_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
		Enabled: true, Principal: "owner", Capabilities: []store.MCPEventCapability{{
			Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"},
		}},
	})
	require.NoError(err)
	pinned, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{
		SourceID: source.ID, SourceReference: pinned.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true,
	}))
	_, binding, err := env.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.source_message_archived", "source", source.ID, []string{"message"}, pinned.Reference)
	require.NoError(err)
	require.NotNil(binding)

	permission := newMutableGmailReadPermission(true)
	client := gmailPermissionReductionAfterRequestAPI{
		MockAPI: env.Mock, permission: permission, operation: "labels", unknown: true,
	}
	worker := New(client, env.Store, nil)
	_, err = worker.Incremental(t.Context(), source)
	require.ErrorIs(err, errNativeSourcePermissionReduction, "the sync must pause when a successful refresh makes read permission unknown")

	allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed, "unknown refresh metadata must clear the persisted permission evidence")
	current, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.Equal(pinned.Reference, current.Reference, "unknown permission pauses Events without retiring the source lifetime")
	require.Error(env.Store.CheckMCPResourceBinding(t.Context(), binding), "unknown permission must pause existing bindings")
	_, _, err = env.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.source_message_archived", "source", source.ID, []string{"message"}, current.Reference)
	require.Error(err, "unknown permission must pause new subscriptions on the retained lifetime")
	assert.Equal("native_authority_unavailable", err.Error())
	require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{
		SourceID: source.ID, SourceReference: pinned.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true,
	}), "permission restoration must reuse the unchanged source lifetime")
	require.NoError(env.Store.CheckMCPResourceBinding(t.Context(), binding), "the existing binding remains valid after permission returns")

	currentSource, err := env.Store.GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(err)
	assert.Equal("1000", currentSource.SyncCursor.String, "the incomplete permission observation must leave history retryable")
}

func TestGmailProfileFailureAfterGrantReductionRevokesEventsAuthority(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "1000")
	_, err := env.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{
		Enabled: true, Principal: "owner", Capabilities: []store.MCPEventCapability{{
			Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"},
		}},
	})
	require.NoError(err)
	pinned, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	require.NoError(env.Store.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{
		SourceID: source.ID, SourceReference: pinned.Reference, SourceType: "gmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true,
	}))
	_, binding, err := env.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.source_message_archived", "source", source.ID, []string{"message"}, pinned.Reference)
	require.NoError(err)
	require.NotNil(binding)

	env.Mock.ProfileError = errors.New("temporary profile failure")
	permission := newMutableGmailReadPermission(true)
	client := gmailPermissionReductionAfterRequestAPI{MockAPI: env.Mock, permission: permission, operation: "profile"}
	worker := New(client, env.Store, nil)
	_, err = worker.Full(t.Context(), source.Identifier)
	require.Error(err)

	allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed, "a successful refresh that narrows the grant must pause Events even when profile verification fails")
	current, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.NotEqual(pinned.Reference, current.Reference, "a known grant reduction retires the old source lifetime")
	require.Error(env.Store.CheckMCPResourceBinding(t.Context(), binding), "the old binding must not retain receipt access")
}

type gmailLabelRaceAPI struct {
	grantedGmailProvenanceAPI

	raced bool
}

func (c *gmailLabelRaceAPI) GetMessagesRawBatchWithErrors(ctx context.Context, ids []string) ([]gmail.RawMessageBatchResult, error) {
	if !c.raced {
		c.raced = true
		c.Labels = append(c.Labels, &gmail.Label{ID: "late-user-label", Name: "Late user label", Type: "user"})
		for _, id := range ids {
			if message := c.Messages[id]; message != nil {
				message.LabelIDs = append(message.LabelIDs, "late-user-label")
			}
		}
	}
	return c.MockAPI.GetMessagesRawBatchWithErrors(ctx, ids)
}

func TestIncrementalGmailLabelRaceFailsWithoutAdvancingHistory(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	env := newTestEnv(t)
	env.Mock.Labels = []*gmail.Label{{ID: "INBOX", Name: "INBOX", Type: "system"}}
	source := env.CreateSourceWithHistory(t, "1000")
	env.Mock.AddMessage("synthetic-label-race", testMIME(), []string{"INBOX"})
	env.SetHistory(1001, historyAdded("synthetic-label-race"))
	client := &gmailLabelRaceAPI{grantedGmailProvenanceAPI: grantedGmailProvenanceAPI{env.Mock}}
	worker := New(client, env.Store, nil)

	_, err := worker.Incremental(t.Context(), source)
	require.Error(err, "an unpinned label must fail the run so its history can be retried")
	require.ErrorContains(err, "native Gmail label unavailable")
	assertMessageCount(t, env.Store, 0)
	currentSource, err := env.Store.GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(err)
	assert.Equal("1000", currentSource.SyncCursor.String, "failed ingestion must not advance past the missing label")

	_, err = worker.Incremental(t.Context(), currentSource)
	require.NoError(err, "the next inventory sees the new label and can ingest the message")
	assertMessageCount(t, env.Store, 1)
	currentSource, err = env.Store.GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(err)
	assert.Equal("1001", currentSource.SyncCursor.String)
}
