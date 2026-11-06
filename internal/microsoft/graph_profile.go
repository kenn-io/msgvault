package microsoft

import (
	"context"
	"errors"
	"strings"

	"go.kenn.io/msgvault/internal/msgraph"
)

var ErrGraphMailboxMismatch = errors.New("authenticated Microsoft mailbox does not match the selected account")

// VerifyGraphMailbox uses the ingestion client's current authenticated /me
// evidence. It does not create identities or change archive/OAuth state.
func VerifyGraphMailbox(ctx context.Context, client *msgraph.Client, email string) error {
	var me struct {
		Mail              string   `json:"mail"`
		UserPrincipalName string   `json:"userPrincipalName"`
		ProxyAddresses    []string `json:"proxyAddresses"`
	}
	if err := client.GetJSON(ctx, "/me?$select=mail,userPrincipalName,proxyAddresses", &me); err != nil {
		return err
	}
	addresses := []string{me.Mail, me.UserPrincipalName}
	for _, proxy := range me.ProxyAddresses {
		if len(proxy) > 5 && strings.EqualFold(proxy[:5], "smtp:") {
			addresses = append(addresses, proxy[5:])
		}
	}
	for _, address := range addresses {
		if email != "" && strings.EqualFold(address, email) {
			return nil
		}
	}
	return ErrGraphMailboxMismatch
}
