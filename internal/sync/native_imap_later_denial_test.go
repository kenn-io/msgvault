package sync

import (
	"context"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	imapclient "go.kenn.io/msgvault/internal/imap"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
	"golang.org/x/oauth2"
)

// This adapter injects protocol authentication failures at later requests,
// after the real Syncer has already established the IMAP source lifetime.
type nativeIMAPLaterDenialAPI struct {
	*gmail.MockAPI

	identity     string
	inventoryErr error
	labelsErr    error
	batchErr     error
}

func (c *nativeIMAPLaterDenialAPI) NativeMailboxSourceIdentity() string  { return c.identity }
func (c *nativeIMAPLaterDenialAPI) NativeMailboxInventoryComplete() bool { return true }
func (c *nativeIMAPLaterDenialAPI) NativeMailboxInventory(context.Context) (string, []imapclient.NativeMailboxScope, []string, error) {
	if c.inventoryErr != nil {
		return c.identity, nil, nil, c.inventoryErr
	}
	return c.identity, []imapclient.NativeMailboxScope{{Mailbox: "INBOX", UIDValidity: 1}}, nil, nil
}
func (*nativeIMAPLaterDenialAPI) NativeMailboxSelectedEpochs() map[string]uint32 { return nil }
func (*nativeIMAPLaterDenialAPI) NativeMailboxMemberships(string, []byte) ([]imapclient.NativeMailboxScope, error) {
	return nil, nil
}
func (c *nativeIMAPLaterDenialAPI) ListLabels(ctx context.Context) ([]*gmail.Label, error) {
	if c.labelsErr != nil {
		return nil, c.labelsErr
	}
	return c.MockAPI.ListLabels(ctx)
}
func (c *nativeIMAPLaterDenialAPI) GetMessagesRawBatchWithErrors(ctx context.Context, ids []string) ([]gmail.RawMessageBatchResult, error) {
	if c.batchErr != nil {
		results := make([]gmail.RawMessageBatchResult, len(ids))
		for i, id := range ids {
			results[i] = gmail.RawMessageBatchResult{ID: id, Err: c.batchErr}
		}
		return results, nil
	}
	return c.MockAPI.GetMessagesRawBatchWithErrors(ctx, ids)
}

func TestNativeIMAPLaterAuthenticationDenialsRetireEventsAuthority(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stage  string
		denial error
	}{
		{name: "label listing", stage: "listing", denial: &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeAuthenticationFailed}},
		{name: "mailbox inventory", stage: "inventory", denial: &imaplib.Error{Type: imaplib.StatusResponseTypeNo, Code: imaplib.ResponseCodeAuthorizationFailed}},
		{name: "batch fetch reconnect", stage: "batch", denial: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			env := newTestEnv(t)
			configureNativeIMAPEventCapture(t, env.Store)
			const route = "imap://synthetic@example.test"
			api := &nativeIMAPLaterDenialAPI{MockAPI: env.Mock, identity: route}
			opts := DefaultOptions()
			opts.SourceType = sourceTypeIMAP
			opts.NoResume = true
			worker := New(api, env.Store, opts)
			_, err := worker.Full(t.Context(), route)
			require.NoError(err)
			source, err := env.Store.GetSourceByTypeAndIdentifier(sourceTypeIMAP, route)
			require.NoError(err)
			pinned, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			allowed, err := env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			require.True(allowed, "the first complete native inventory establishes read authority")
			_, binding, err := env.Store.ResolveMCPSubscriptionResource(t.Context(), "msgvault.source_message_archived", "source", source.ID, []string{"message"}, pinned.Reference)
			require.NoError(err)
			require.NotNil(binding)

			switch tc.stage {
			case "listing":
				api.labelsErr = tc.denial
			case "inventory":
				api.inventoryErr = tc.denial
			case "batch":
				api.AddMessage("synthetic-denied-batch", testemail.NewMessage().From("sender@example.test").To("owner@example.test").Body("Synthetic batch message").Bytes(), []string{"INBOX"})
				api.MessagePages = [][]string{{"synthetic-denied-batch"}}
				api.batchErr = tc.denial
			}

			_, err = worker.Full(t.Context(), route)
			if tc.stage == "batch" {
				require.NoError(err, "individual batch failures remain best effort after revocation is persisted")
			} else {
				require.Error(err)
			}
			allowed, err = env.Store.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a later native authentication denial must retire the pinned grant")
			current, err := env.Store.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.NotEqual(pinned.Reference, current.Reference, "old source authority must not be restored by the failed provider request")
			require.Error(env.Store.CheckMCPResourceBinding(t.Context(), binding), "existing receipt bindings must not survive the denial")
		})
	}
}
