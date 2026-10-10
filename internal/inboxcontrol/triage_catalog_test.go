package inboxcontrol_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type triageCatalogProvider struct{ controlProvider }

func (triageCatalogProvider) TagCatalog(context.Context, inboxcontrol.SourceIdentity) ([]emailtags.Tag, error) {
	return []emailtags.Tag{{ID: "Todo", Name: "Todo"}}, nil
}

func TestUpdateTriageMappingsMarksNativeCatalogResolution(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	archive := storetest.New(t)
	sourceRow, err := archive.Store.GetOrCreateSource("msmail", "mailbox@example.test")
	requirements.NoError(err)
	source := inboxcontrol.SourceIdentity{SourceID: sourceRow.ID, SourceType: "msmail", SourceIdentifier: sourceRow.Identifier, AccountID: sourceRow.Identifier}
	provider := &triageCatalogProvider{}
	var resolved inboxcontrol.Request
	service := &inboxcontrol.Service{
		Ledger:    archive.Store,
		Authorize: func(context.Context, inboxcontrol.Principal, inboxcontrol.Request) error { return nil },
		Resolve: func(_ context.Context, request inboxcontrol.Request) (inboxcontrol.Provider, error) {
			resolved = request
			return provider, nil
		},
		AcquireSource: func(ctx context.Context, sourceID int64) (func(), error) {
			execution, err := archive.Store.AcquireSyncExecutionContext(ctx, sourceID)
			if err != nil {
				return nil, err
			}
			return func() { _ = execution.Release() }, nil
		},
	}
	principal := inboxcontrol.Principal{ID: "owner-fixture", Owner: true}
	revision, err := service.UpdateTriageMappings(t.Context(), source, map[string]string{"todo": "Todo"}, 0, principal, func(context.Context) (func(), error) { return func() {}, nil })
	requirements.NoError(err)
	assertions.Equal(int64(1), revision)
	assertions.True(resolved.NativeTagCatalog)
	assertions.Equal(inboxcontrol.OpGetCapabilities, resolved.Operation)
}
