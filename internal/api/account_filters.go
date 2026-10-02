package api

import (
	"encoding/json/v2"
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/search"
)

func parseAccountScopes(r *http.Request) ([]search.AccountScope, error) {
	var scopes []search.AccountScope
	if encoded := r.URL.Query().Get("account_scopes"); encoded != "" {
		if len(encoded) > 32768 {
			return nil, newParamError("account_scopes", "account scopes exceed 32 KiB")
		}
		if err := json.Unmarshal([]byte(encoded), &scopes, json.RejectUnknownMembers(true)); err != nil {
			return nil, newParamError("account_scopes", fmt.Sprintf("invalid account scopes: %v", err))
		}
	}
	var plain search.AccountScope
	for _, a := range r.URL.Query()["account_addresses"] {
		s, err := search.ParseAccountSelector(a, true)
		if err != nil || len(s.Addresses) != 1 {
			return nil, newParamError("account_addresses", "expected exact email addresses")
		}
		plain.Addresses = append(plain.Addresses, s.Addresses...)
	}
	for _, g := range r.URL.Query()["account_groups"] {
		s, err := search.ParseAccountSelector(g, false)
		if err != nil || len(s.Groups) != 1 {
			return nil, newParamError("account_groups", "expected identity group keys")
		}
		plain.Groups = append(plain.Groups, s.Groups...)
	}
	if value := r.URL.Query().Get("account_unattributed"); value != "" {
		if value != "true" && value != "false" {
			return nil, newParamError("account_unattributed", "expected true or false")
		}
		plain.Unattributed = value == "true"
	}
	if plain.Unattributed && len(plain.Addresses)+len(plain.Groups) > 0 {
		return nil, newParamError("account_unattributed", "unattributed cannot be combined with account addresses or groups")
	}
	if plain.Unattributed || len(plain.Addresses)+len(plain.Groups) > 0 {
		scopes = append(scopes, plain)
	}
	if err := search.ValidateAccountScopes(scopes); err != nil {
		return nil, newParamError("account_scopes", err.Error())
	}
	return scopes, nil
}
