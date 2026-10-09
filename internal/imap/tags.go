package imap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

func keywordTags(flags []string) []string {
	tags := make([]string, 0, len(flags))
	for _, flag := range flags {
		if !strings.HasPrefix(flag, "\\") {
			tags = append(tags, flag)
		}
	}
	slices.Sort(tags)
	return tags
}

// fetchKeywordFlags tracks FETCH item presence: FLAGS () is valid, omitted FLAGS is not.
func fetchKeywordFlags(conn *imapclient.Client, uid uint32) ([]string, error) {
	var set imaplib.UIDSet
	set.AddNum(imaplib.UID(uid))
	cmd := conn.Fetch(set, &imaplib.FetchOptions{UID: true, Flags: true})
	var flags []string
	found := false
	complete := false
	for message := cmd.Next(); message != nil; message = cmd.Next() {
		var actual imaplib.UID
		hasUID, hasFlags := false, false
		var current []string
		for item := message.Next(); item != nil; item = message.Next() {
			switch data := item.(type) {
			case imapclient.FetchItemDataUID:
				actual = data.UID
				hasUID = true
			case imapclient.FetchItemDataFlags:
				hasFlags = true
				current = make([]string, len(data.Flags))
				for i, flag := range data.Flags {
					current[i] = string(flag)
				}
			}
		}
		if hasUID && uint32(actual) == uid {
			found = true
			complete = hasFlags
			flags = current
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("fetch IMAP keyword flags: %w", err)
	}
	if !found {
		return nil, emailtags.Failure("stale_identity", "IMAP message moved or disappeared; sync the account before retrying", nil, nil)
	}
	if !complete {
		return nil, emailtags.Failure("provider_read_failed", "IMAP FETCH omitted FLAGS; no complete tag state is available", nil, nil)
	}
	slices.Sort(flags)
	return flags, nil
}

// MessageKeywords reads or changes custom keyword flags on one exact mailbox copy.
func (c *Client) MessageKeywords(ctx context.Context, id DraftReceipt, input *emailtags.MessageTagChange) (*emailtags.MessageTagResult, error) {
	if id.Mailbox == "" || id.UIDValidity == 0 || id.UID == 0 {
		return nil, emailtags.Failure("stale_identity", "IMAP tags require a stored mailbox UID and UIDVALIDITY; sync the account first", nil, nil)
	}
	var change emailtags.MessageTagChange
	if input != nil {
		change = *input
	}
	result := &emailtags.MessageTagResult{Tags: []string{}, Before: []string{}, AvailableTags: []emailtags.MessageTag{}, Provider: "imap", Mailbox: id.Mailbox, UIDValidity: id.UIDValidity, UID: id.UID}
	err := c.withDraftConn(ctx, func(conn *imapclient.Client) error {
		selected, err := conn.Select(id.Mailbox, nil).Wait()
		if err != nil {
			return emailtags.Failure("provider_read_failed", "cannot select IMAP mailbox; check access and sync the account", result, err)
		}
		c.selectedMailbox = id.Mailbox
		c.selectedUIDValidity = selected.UIDValidity
		c.selectedNumMessages = selected.NumMessages
		if selected.UIDValidity != id.UIDValidity {
			return emailtags.Failure("stale_identity", "IMAP UIDVALIDITY changed; sync the account before retrying", result, nil)
		}
		permanent := make([]string, len(selected.PermanentFlags))
		for i, flag := range selected.PermanentFlags {
			permanent[i] = string(flag)
		}
		// RFC 9051 treats an omitted PERMANENTFLAGS as all flags permanent; an explicit "()" allows none.
		result.CanCreateKeywords = selected.PermanentFlags == nil || emailtags.Contains(permanent, string(imaplib.FlagWildcard), true)
		for _, tag := range keywordTags(permanent) {
			result.AvailableTags = append(result.AvailableTags, emailtags.MessageTag{ID: tag, Name: tag})
		}
		flags, err := fetchKeywordFlags(conn, id.UID)
		if err != nil {
			return err
		}
		result.Flags = flags
		result.Tags = keywordTags(flags)
		result.Before = slices.Clone(result.Tags)
		if input == nil {
			result.Verified = true
			return nil
		}
		result.DryRun = change.DryRun
		// Even a satisfied change needs permanent keyword support: session flags
		// cannot prove the requested tag is persisted for another mail client.
		for _, tag := range append(slices.Clone(change.Add), change.Remove...) {
			if !result.CanCreateKeywords && !emailtags.Contains(permanent, tag, true) {
				return emailtags.Failure("unsupported_keywords", "IMAP mailbox does not advertise persistent support for this keyword", result, nil)
			}
		}
		if change.DryRun {
			result.Tags = emailtags.Project(result.Tags, change, true)
			return nil
		}
		add, remove := emailtags.Delta(result.Tags, change, true)
		var set imaplib.UIDSet
		set.AddNum(imaplib.UID(id.UID))
		storeFlags := func(tags []string, op imaplib.StoreFlagsOp) error {
			if len(tags) == 0 {
				return nil
			}
			values := make([]imaplib.Flag, len(tags))
			for i, tag := range tags {
				values[i] = imaplib.Flag(tag)
			}
			_, err := conn.Store(set, &imaplib.StoreFlags{Op: op, Silent: true, Flags: values}, nil).Collect()
			if err != nil {
				return fmt.Errorf("store IMAP keywords: %w", err)
			}
			return nil
		}
		writeErr := storeFlags(add, imaplib.StoreFlagsAdd)
		if writeErr == nil {
			writeErr = storeFlags(remove, imaplib.StoreFlagsDel)
		}
		if len(add)+len(remove) > 0 {
			after, readErr := fetchKeywordFlags(conn, id.UID)
			if readErr == nil {
				result.Flags = after
				result.Tags = keywordTags(after)
			}
			if writeErr != nil {
				return emailtags.Failure("remote_unknown", "IMAP tag changes may have partially applied; read current tags before retrying", result, errors.Join(writeErr, readErr))
			}
			if readErr != nil {
				return emailtags.Failure("remote_unknown", "IMAP accepted the write but readback failed; read tags and sync before retrying", result, readErr)
			}
		}
		if !emailtags.Verify(result.Tags, change, true) {
			return emailtags.Failure("verification_failed", "IMAP readback does not match requested changes; inspect tags before retrying", result, nil)
		}
		result.Verified = true
		return nil
	})
	if err != nil {
		if result.Flags == nil {
			result = nil
		}
		if failure, ok := errors.AsType[*emailtags.MessageTagError](err); ok {
			failure.Result = result
			return result, failure
		}
		return result, emailtags.Failure("provider_read_failed", fmt.Sprintf("cannot read tags in mailbox %q; check account access and retry", id.Mailbox), result, err)
	}
	return result, nil
}
