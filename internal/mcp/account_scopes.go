package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// accountSelection is a resolved account argument: a physical source, a
// virtual account of one source, or an address matched on every source.
type accountSelection struct {
	sourceID *int64
	scope    *search.AccountScope
}

var errAccountSelectionConflict = errors.New(
	"the account argument and the query's account: operator select different addresses")

// resolveAccount reads the account argument. A physical source identifier
// keeps its old meaning; a virtual account key from get_stats selects one
// identity or the unattributed rows of its source; the exact address of a
// confirmed identity selects mail attributed to it on every source. Any other
// value is "account not found", so a typo never silently matches nothing.
func (h *handlers) resolveAccount(ctx context.Context, account string) (accountSelection, error) {
	if account == "" {
		return accountSelection{}, nil
	}
	if _, scopes, err := virtualAccountScopes(account); err != nil {
		return accountSelection{}, err
	} else if len(scopes) > 0 {
		sourceID := *scopes[0].SourceID
		accounts, err := h.engine.ListAccounts(ctx)
		if err != nil {
			return accountSelection{}, newInternalError("list accounts", err)
		}
		if !slices.ContainsFunc(accounts, func(a query.AccountInfo) bool { return a.ID == sourceID }) {
			return accountSelection{}, &expectedHandlerError{message: "account not found: " + account}
		}
		return accountSelection{sourceID: &sourceID, scope: &scopes[0]}, nil
	}
	sourceID, err := h.getAccountID(ctx, account)
	if err == nil {
		return accountSelection{sourceID: sourceID}, nil
	}
	if expected, ok := errors.AsType[*expectedHandlerError](err); !ok || !strings.HasPrefix(expected.message, "account not found") {
		return accountSelection{}, err
	}
	address, parseErr := search.ParseAccountAddress(account)
	if parseErr != nil {
		return accountSelection{}, err
	}
	known, knownErr := h.isConfirmedIdentity(ctx, address)
	if knownErr != nil {
		return accountSelection{}, newInternalError("list virtual accounts", knownErr)
	}
	if !known {
		return accountSelection{}, err
	}
	return accountSelection{scope: &search.AccountScope{Addresses: []string{address}}}, nil
}

// isConfirmedIdentity reports whether any source lists address as a confirmed
// identity in the virtual account catalog.
func (h *handlers) isConfirmedIdentity(ctx context.Context, address string) (bool, error) {
	lister, ok := h.engine.(query.VirtualAccountLister)
	if !ok {
		return false, nil
	}
	catalog, err := lister.ListVirtualAccounts(ctx)
	if err != nil {
		return false, err
	}
	for _, children := range catalog {
		if slices.ContainsFunc(children, func(v store.VirtualAccount) bool {
			return !v.Unattributed && v.AccountAddress == address
		}) {
			return true, nil
		}
	}
	return false, nil
}

// scopes returns the selection as filter scopes.
func (a accountSelection) scopes() []search.AccountScope {
	if a.scope == nil {
		return nil
	}
	return search.CloneAccountScopes([]search.AccountScope{*a.scope})
}

// applyToFilter narrows a message filter to the selection.
func (a accountSelection) applyToFilter(filter *query.MessageFilter) {
	filter.SourceID = a.sourceID
	filter.AccountScopes = append(filter.AccountScopes, a.scopes()...)
}

// applyToQuery narrows a parsed query. An address selection becomes the
// account: operator; the unattributed bucket, which no operator can express,
// rides the query's structured scopes.
func (a accountSelection) applyToQuery(q *search.Query) error {
	if a.sourceID != nil {
		q.AccountIDs = []int64{*a.sourceID}
	}
	if a.scope == nil {
		return nil
	}
	if a.scope.Unattributed {
		q.AccountScopes = append(q.AccountScopes, a.scopes()...)
		return nil
	}
	if len(q.AccountAddrs) == 0 {
		q.AccountAddrs = slices.Clone(a.scope.Addresses)
		return nil
	}
	// account: values are alternatives, so the selection must intersect them.
	var kept []string
	for _, address := range q.AccountAddrs {
		if slices.Contains(a.scope.Addresses, address) {
			kept = append(kept, address)
		}
	}
	if len(kept) == 0 {
		return errAccountSelectionConflict
	}
	q.AccountAddrs = kept
	return nil
}

// resolveForwardedAccount resolves the account argument for requests the
// daemon answers: a physical source stays an account name, and anything else
// becomes a scope.
func (h *handlers) resolveForwardedAccount(ctx context.Context, account string) (string, []search.AccountScope, error) {
	if account == "" || h.engine == nil {
		return virtualAccountScopes(account)
	}
	selection, err := h.resolveAccount(ctx, account)
	if err != nil {
		return "", nil, err
	}
	if selection.scope == nil {
		return account, nil, nil
	}
	return "", selection.scopes(), nil
}

// virtualAccountScopes turns a virtual account key into a scope for paths
// that hand the account to the daemon, which resolves physical sources
// itself. Any other account value is returned unchanged.
func virtualAccountScopes(account string) (string, []search.AccountScope, error) {
	if !store.IsVirtualAccountKey(account) {
		return account, nil, nil
	}
	sourceID, address, unattributed, err := store.ParseVirtualAccountKey(account)
	if err != nil {
		return "", nil, &expectedHandlerError{message: err.Error()}
	}
	scope := search.AccountScope{SourceID: &sourceID, Unattributed: unattributed}
	if !unattributed {
		if address, err = search.ParseAccountAddress(address); err != nil {
			return "", nil, &expectedHandlerError{message: "invalid virtual account key: " + account}
		}
		scope.Addresses = []string{address}
	}
	return "", []search.AccountScope{scope}, nil
}
