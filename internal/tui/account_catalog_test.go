package tui

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

// flakyCatalogEngine reports the virtual account catalog unavailable until
// available is set, as a daemon does while its first catalog read is slow.
// listErr fails the account list itself.
type flakyCatalogEngine struct {
	*querytest.MockEngine

	available bool
	listErr   error
}

func (e *flakyCatalogEngine) ListAccounts(ctx context.Context) ([]query.AccountInfo, error) {
	if e.listErr != nil {
		return nil, e.listErr
	}
	accounts, err := e.MockEngine.ListAccounts(ctx)
	// Copy so the model never shares the engine's slice.
	return append([]query.AccountInfo(nil), accounts...), err
}

func (e *flakyCatalogEngine) ListVirtualAccounts(ctx context.Context) (map[int64][]store.VirtualAccount, error) {
	if !e.available {
		return nil, errors.New("virtual account catalog unavailable")
	}
	return e.MockEngine.ListVirtualAccounts(ctx)
}

var catalogChildren = []store.VirtualAccount{
	{Key: "identity:7:work", SourceID: 7, AccountAddress: "work@example.org", MessageCount: 3},
	{Key: "unattributed:7", SourceID: 7, Unattributed: true, MessageCount: 2},
}

func newCatalogModel(t *testing.T, accounts []query.AccountInfo) (Model, *flakyCatalogEngine) {
	t.Helper()
	engine := &flakyCatalogEngine{MockEngine: &querytest.MockEngine{
		Accounts:        accounts,
		VirtualAccounts: map[int64][]store.VirtualAccount{7: catalogChildren},
	}}
	return New(engine, Options{DataDir: t.TempDir(), Version: "test"}), engine
}

// loadAccountsOnce runs one account read through Update and returns the
// model and its follow-up command, if any.
func loadAccountsOnce(t *testing.T, model Model) (Model, tea.Cmd) {
	t.Helper()
	updated, cmd := model.Update(model.loadAccounts()())
	return asModel(t, updated), cmd
}

func TestAccountCatalogRecoversWithinSession(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	model, engine := newCatalogModel(t, []query.AccountInfo{{ID: 7, Identifier: "inbox@example.net"}})

	model, retry := loadAccountsOnce(t, model)
	require.NotNil(retry, "an unavailable catalog schedules a reread")
	assert.Empty(model.accounts[0].VirtualAccounts)

	// A reread that fails outright keeps to the backoff instead of stopping.
	engine.listErr = errors.New("daemon restarting")
	updated, _ := model.Update(accountCatalogRetryMsg{})
	model = asModel(t, updated)
	model, retry = loadAccountsOnce(t, model)
	require.NotNil(retry, "a failed reread during recovery schedules another")

	engine.listErr, engine.available = nil, true
	model, retry = loadAccountsOnce(t, model)
	assert.Nil(retry, "a readable catalog schedules nothing more")
	assert.Equal(catalogChildren, model.accounts[0].VirtualAccounts)

	// A later unavailable read keeps the receiving addresses already shown.
	engine.available = false
	model, _ = loadAccountsOnce(t, model)
	assert.Equal(catalogChildren, model.accounts[0].VirtualAccounts)
}

func TestAccountCatalogKeepsCachedChildrenAtStartup(t *testing.T) {
	// The daemon passes along its last known children when the catalog is
	// unavailable; startup must show them rather than nothing.
	cached := []store.VirtualAccount{catalogChildren[0]}
	model, _ := newCatalogModel(t, []query.AccountInfo{{ID: 7, Identifier: "inbox@example.net", VirtualAccounts: cached}})
	model, _ = loadAccountsOnce(t, model)
	assert.Equal(t, cached, model.accounts[0].VirtualAccounts)
}

func TestAccountCatalogRefreshKeepsSelectorHighlight(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	model, engine := newCatalogModel(t, []query.AccountInfo{
		{ID: 7, Identifier: "inbox@example.net"},
		{ID: 9, Identifier: "other@example.net"},
	})
	model, _ = loadAccountsOnce(t, model)
	model.openAccountSelector()
	options := model.selectorOptions()
	for i, option := range options {
		if option.kind == scopeOptionAccount && *option.accountID == 9 {
			model.modalCursor = i
		}
	}
	require.Equal(int64(9), *options[model.modalCursor].accountID)

	// The recovered catalog inserts source 7's children above source 9.
	engine.available = true
	model, _ = loadAccountsOnce(t, model)
	highlighted := model.selectorOptions()[model.modalCursor]
	require.Equal(scopeOptionAccount, highlighted.kind)
	assert.Equal(int64(9), *highlighted.accountID, "the highlight stays on the same account")
}
