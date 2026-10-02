package provideridentity

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

// GmailProfileEvidence binds an authenticated profile to its selected archive
// source. Callers decide whether to preview, confirm, or refresh ownership.
func GmailProfileEvidence(source *store.Source, profileAddress string) ([]identityops.ExternalEvidence, error) {
	if source == nil || source.SourceType != "gmail" {
		return nil, errors.New("identity evidence requires a Gmail source")
	}
	address := strings.TrimSpace(profileAddress)
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address || !strings.Contains(address, "@") || !oauth.SameGoogleAccount(source.Identifier, address) {
		return nil, fmt.Errorf("authenticated Gmail profile does not match archive source %d", source.ID)
	}
	return []identityops.ExternalEvidence{{Identifier: address, Signal: "oauth", Strong: true}}, nil
}
