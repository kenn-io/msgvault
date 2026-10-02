package mcp

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// resolveAccountScope preserves physical source identifiers and accepts stable
// virtual-account keys or a unique catalog identity/group. It never picks one
// of several physical sources with the same selector.
func (h *handlers) resolveAccountScope(ctx context.Context, account string) (*int64, []search.AccountScope, error) {
	account = strings.TrimSpace(account)
	if account == "" {
		return nil, nil, nil
	}
	var accounts []query.AccountInfo
	var err error
	lister, sourceOnly := h.engine.(query.SourceAccountLister)
	if sourceOnly {
		accounts, err = lister.ListSourceAccounts(ctx)
	} else {
		accounts, err = h.engine.ListAccounts(ctx)
	}
	if err != nil {
		return nil, nil, newInternalError("list accounts", err)
	}
	var id *int64
	var scope *search.AccountScope
	for _, a := range accounts {
		if strings.EqualFold(a.Identifier, account) || strings.EqualFold(a.DisplayName, account) {
			if id != nil {
				return nil, nil, &expectedHandlerError{message: "account matches multiple sources: " + account}
			}
			n := a.ID
			id = &n
		}
	}
	if id != nil {
		return id, nil, nil
	}
	if strings.HasPrefix(account, "group:") || strings.HasPrefix(account, "identity:") || strings.HasPrefix(account, "unattributed:") {
		source, address, unattributed, err := store.ParseVirtualAccountKey(account)
		if err != nil {
			return nil, nil, &expectedHandlerError{message: err.Error()}
		}
		exists := false
		for _, a := range accounts {
			exists = exists || a.ID == source
		}
		if !exists {
			return nil, nil, &expectedHandlerError{message: "account source not found"}
		}
		s := search.AccountScope{Unattributed: unattributed}
		if !unattributed {
			s, err = search.ParseAccountSelector(address, false)
			if err != nil {
				return nil, nil, &expectedHandlerError{message: err.Error()}
			}
		}
		s.SourceID = &source
		return &source, []search.AccountScope{s}, nil
	}
	// Unqualified aliases need the catalog to detect cross-source ambiguity.
	if sourceOnly {
		accounts, err = h.engine.ListAccounts(ctx)
		if err != nil {
			return nil, nil, newInternalError("list virtual accounts", err)
		}
	}
	for _, a := range accounts {
		for _, v := range a.VirtualAccounts {
			if !strings.EqualFold(v.AccountAddress, account) && v.Group != strings.ToLower(account) {
				continue
			}
			if id != nil {
				return nil, nil, &expectedHandlerError{message: fmt.Sprintf("account matches multiple sources: %s; use its virtual account key", account)}
			}
			n := a.ID
			id = &n
			s := search.AccountScope{SourceID: &n, Unattributed: v.Unattributed}
			if v.Group != "" {
				s.Groups = []string{v.Group}
			} else {
				s.Addresses = []string{v.AccountAddress}
			}
			scope = &s
		}
	}
	if id != nil {
		return id, []search.AccountScope{*scope}, nil
	}
	return nil, nil, &expectedHandlerError{message: "account not found: " + account}
}
