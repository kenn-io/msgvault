package api

import (
	"encoding/json/v2"
	"fmt"
	"net/http"

	"go.kenn.io/msgvault/internal/search"
)

const maxAccountScopesParamBytes = 32 << 10

// parseAccountScopes reads account_scopes, a JSON list of scopes that
// intersect.
func parseAccountScopes(r *http.Request) ([]search.AccountScope, error) {
	var scopes []search.AccountScope
	if encoded := r.URL.Query().Get("account_scopes"); encoded != "" {
		if len(encoded) > maxAccountScopesParamBytes {
			return nil, newParamError("account_scopes", "account scopes exceed 32 KiB")
		}
		if err := json.Unmarshal([]byte(encoded), &scopes, json.RejectUnknownMembers(true)); err != nil {
			return nil, newParamError("account_scopes", fmt.Sprintf("invalid account scopes: %v", err))
		}
	}
	if err := search.ValidateAccountScopes(scopes); err != nil {
		return nil, newParamError("account_scopes", err.Error())
	}
	return scopes, nil
}
