package imap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	imapapi "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// ValidateArchiveMailbox checks an explicit existing mailbox with metadata-only
// LIST and EXAMINE. It does not create a mailbox or change any message flags.
func (c *Client) ValidateArchiveMailbox(ctx context.Context, name string) error {
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) || strings.EqualFold(name, "INBOX") {
		return errors.New("archive mailbox must name an existing mailbox other than INBOX")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("archive mailbox contains a control character")
		}
	}
	return c.withDraftConn(ctx, func(conn *imapclient.Client) error {
		mailboxes, err := conn.List("", "*", nil).Collect()
		if err != nil {
			return fmt.Errorf("list archive mailbox: %w", err)
		}
		found := false
		for _, mailbox := range mailboxes {
			if mailbox.Mailbox == name && !slices.Contains(mailbox.Attrs, imapapi.MailboxAttrNoSelect) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("archive mailbox %q does not exist or is not selectable", name)
		}
		selected, err := conn.Select(name, &imapapi.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return fmt.Errorf("archive mailbox %q is not selectable: %w", name, err)
		}
		if selected.UIDValidity == 0 {
			return fmt.Errorf("archive mailbox %q has no valid UID epoch", name)
		}
		c.selectedMailbox, c.selectedUIDValidity, c.selectedNumMessages = name, selected.UIDValidity, selected.NumMessages
		return nil
	})
}
