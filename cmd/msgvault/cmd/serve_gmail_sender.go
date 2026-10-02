package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/mail"
	"strings"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

func (a *storeAPIAdapter) readGmailDraftSendAs(ctx context.Context, source *store.Source) ([]gmail.SendAs, error) {
	if err := gmailDraftScopeGate(ctx, a.config, source, oauth.ScopesGmailSendAsList); err != nil {
		return nil, err
	}
	factory := a.gmailDraftClientFactory
	if factory == nil {
		factory = defaultGmailDraftClientFactory
	}
	client, err := factory(ctx, source)
	if err != nil {
		return nil, draftReplyError("invalid_source", err)
	}
	defer func() { _ = client.Close() }()
	entries, err := client.ListSendAs(ctx)
	if err != nil {
		return nil, draftReplyError(gmailReadErrorCode(err), err)
	}
	return entries, nil
}

// addressedGmailDraftSender prefers visible recipients to delivery trace
// headers, which often contain the primary inbox after alias forwarding.
// Headers only select among owner-confirmed, provider-accepted identities.
func addressedGmailDraftSender(raw []byte, confirmed map[string]string, entries []gmail.SendAs) (string, error) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", draftReplyError("invalid_reply_metadata", errors.New("read parent headers"))
	}
	for _, names := range [][]string{{"To", "Cc"}, {"Delivered-To", "X-Original-To"}} {
		matches := make(map[string]string)
		for _, name := range names {
			for _, value := range message.Header[name] {
				addresses, err := mail.ParseAddressList(value)
				if err != nil {
					// Some delivery trace values are not valid address lists.
					// Ignore unusable values and keep the existing fallback.
					if name == "Delivered-To" || name == "X-Original-To" {
						continue
					}
					return "", draftReplyError("invalid_reply_metadata", errors.New("invalid parent recipient header"))
				}
				for _, address := range addresses {
					key := store.NormalizeIdentifierForCompare(address.Address)
					if sender, ok := confirmed[key]; ok && validateGmailSendAs(entries, address.Address) == nil {
						matches[key] = sender
					}
				}
			}
		}
		if len(matches) > 1 {
			return "", draftReplyError("from_ambiguous", errors.New("multiple confirmed Gmail senders match parent recipients; choose --from"))
		}
		for _, sender := range matches {
			return sender, nil
		}
	}
	return "", nil
}

// parseSendAsConfirmations removes repeatable owner confirmations before the
// existing account/list parser runs. Validate the whole list before writing.
func parseSendAsConfirmations(args []string) ([]string, []string, error) {
	rest := make([]string, 0, len(args))
	var confirmations []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--confirm" {
			i++
			if i == len(args) {
				return nil, nil, draftReplyError("invalid_args", errors.New("--confirm requires an address"))
			}
			arg = "--confirm=" + args[i]
		}
		if value, ok := strings.CutPrefix(arg, "--confirm="); ok {
			address, _, err := parseDraftSender(value)
			if err != nil {
				return nil, nil, draftReplyError("invalid_from", err)
			}
			confirmations = append(confirmations, address.Address)
		} else {
			rest = append(rest, arg)
		}
	}
	return rest, confirmations, nil
}
