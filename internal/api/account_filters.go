package api

import (
	"encoding/json/v2"
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/search"
)

const maxAccountScopesParamBytes = 32 << 10

// parseAccountScopes reads the structured account filters: account_scopes is
// a JSON list of scopes that intersect, and account_addresses with
// account_unattributed form one more scope whose values are alternatives.
func parseAccountScopes(r *http.Request) ([]search.AccountScope, error) {
	values := r.URL.Query()
	var scopes []search.AccountScope
	if encoded := values.Get("account_scopes"); encoded != "" {
		if len(encoded) > maxAccountScopesParamBytes {
			return nil, newParamError("account_scopes", "account scopes exceed 32 KiB")
		}
		if err := json.Unmarshal([]byte(encoded), &scopes, json.RejectUnknownMembers(true)); err != nil {
			return nil, newParamError("account_scopes", fmt.Sprintf("invalid account scopes: %v", err))
		}
	}
	var plain search.AccountScope
	for _, value := range values["account_addresses"] {
		address, err := search.ParseAccountAddress(value)
		if err != nil {
			return nil, newParamError("account_addresses", "expected exact email addresses")
		}
		plain.Addresses = append(plain.Addresses, address)
	}
	if value := values.Get("account_unattributed"); value != "" {
		if value != "true" && value != "false" {
			return nil, newParamError("account_unattributed", "expected true or false")
		}
		plain.Unattributed = value == "true"
	}
	if plain.Unattributed || len(plain.Addresses) > 0 {
		scopes = append(scopes, plain)
	}
	if err := search.ValidateAccountScopes(scopes); err != nil {
		return nil, newParamError("account_scopes", err.Error())
	}
	return scopes, nil
}
