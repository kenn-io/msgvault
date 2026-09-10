package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/mail"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	DraftRemotePresent     = "present"
	DraftRemoteAbsent      = "absent"
	DraftRemoteFlagMissing = "flag_missing"
	DraftRemoteChanged     = "changed"
)

// DraftTarget identifies a specific remote draft by mailbox, epoch, and UID.
type DraftTarget struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	RawSHA256   [32]byte
}

// DraftInspectResult describes the remote state of a draft.
type DraftInspectResult struct {
	State       string
	UIDValidity uint32
	Flags       []string
	RawSHA256   [32]byte
}

// InspectDraft fetches the remote draft and classifies its state.
// It never stores, expunges, or changes any server state.
func (c *Client) InspectDraft(ctx context.Context, target DraftTarget) (DraftInspectResult, error) {
	if err := ValidateDraftMailbox(target.Mailbox); err != nil {
		return DraftInspectResult{}, &DraftAppendError{State: DraftStateRejected, Code: "invalid_mailbox", Err: err}
	}
	var result DraftInspectResult
	err := c.withConn(ctx, func(conn *imapclient.Client) error {
		if err := ctx.Err(); err != nil {
			result = DraftInspectResult{State: DraftStateCancelled}
			return &DraftAppendError{State: DraftStateCancelled, Code: "cancelled", Err: err}
		}
		if !conn.Caps().Has(imap.CapUIDPlus) {
			return &DraftAppendError{State: DraftStateRejected, Code: "uidplus_required",
				Err: errors.New("IMAP server does not advertise UIDPLUS")}
		}
		if err := c.selectMailbox(target.Mailbox); err != nil {
			return err
		}
		if c.selectedUIDValidity != target.UIDValidity {
			return &DraftAppendError{State: DraftStateRejected, Code: "uidvalidity_changed",
				Err: fmt.Errorf("mailbox %q UIDVALIDITY changed from %d to %d",
					target.Mailbox, target.UIDValidity, c.selectedUIDValidity)}
		}
		fetchResult, err := fetchDraftUID(ctx, conn, target.UID)
		if err != nil {
			return err
		}
		if fetchResult == nil {
			result = DraftInspectResult{State: DraftRemoteAbsent, UIDValidity: target.UIDValidity}
			return nil
		}
		result.UIDValidity = target.UIDValidity
		result.Flags = fetchResult.flags
		result.RawSHA256 = fetchResult.digest

		hasDraft := false
		for _, f := range fetchResult.flags {
			if imap.Flag(f) == imap.FlagDraft {
				hasDraft = true
				break
			}
		}
		if !hasDraft {
			result.State = DraftRemoteFlagMissing
			return nil
		}
		if fetchResult.digest != target.RawSHA256 {
			result.State = DraftRemoteChanged
			return nil
		}
		result.State = DraftRemotePresent
		return nil
	})
	if err != nil {
		if appendErr, ok := errors.AsType[*DraftAppendError](err); ok {
			return result, appendErr
		}
		if isNetworkError(err) {
			return DraftInspectResult{State: DraftStateRemoteUnknown}, &DraftAppendError{
				State: DraftStateRemoteUnknown, Code: "remote_unknown", Err: err}
		}
		return DraftInspectResult{}, err
	}
	return result, nil
}

// RemoveDraft permanently deletes a draft from the remote server using
// UID STORE \Deleted + UID EXPUNGE. It only operates when the draft is
// present with the correct \Draft flag and matching digest.
func (c *Client) RemoveDraft(ctx context.Context, target DraftTarget) (DraftInspectResult, error) {
	if err := ValidateDraftMailbox(target.Mailbox); err != nil {
		return DraftInspectResult{}, &DraftAppendError{State: DraftStateRejected, Code: "invalid_mailbox", Err: err}
	}
	var result DraftInspectResult
	err := c.withConn(ctx, func(conn *imapclient.Client) error {
		if err := ctx.Err(); err != nil {
			result = DraftInspectResult{State: DraftStateCancelled}
			return &DraftAppendError{State: DraftStateCancelled, Code: "cancelled", Err: err}
		}
		if !conn.Caps().Has(imap.CapUIDPlus) {
			return &DraftAppendError{State: DraftStateRejected, Code: "uidplus_required",
				Err: errors.New("IMAP server does not advertise UIDPLUS")}
		}
		if err := c.selectMailbox(target.Mailbox); err != nil {
			return err
		}
		if c.selectedUIDValidity != target.UIDValidity {
			return &DraftAppendError{State: DraftStateRejected, Code: "uidvalidity_changed",
				Err: fmt.Errorf("mailbox %q UIDVALIDITY changed from %d to %d",
					target.Mailbox, target.UIDValidity, c.selectedUIDValidity)}
		}
		fetchResult, err := fetchDraftUID(ctx, conn, target.UID)
		if err != nil {
			return err
		}
		if fetchResult == nil {
			result = DraftInspectResult{State: DraftRemoteAbsent, UIDValidity: target.UIDValidity}
			return nil
		}
		result.UIDValidity = target.UIDValidity
		result.Flags = fetchResult.flags
		result.RawSHA256 = fetchResult.digest

		hasDraft := false
		for _, f := range fetchResult.flags {
			if imap.Flag(f) == imap.FlagDraft {
				hasDraft = true
				break
			}
		}
		if !hasDraft {
			result.State = DraftRemoteFlagMissing
			return nil
		}
		if fetchResult.digest != target.RawSHA256 {
			result.State = DraftRemoteChanged
			return nil
		}
		result.State = DraftRemotePresent
		if err := expungeUIDLocked(conn, target.UID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if appendErr, ok := errors.AsType[*DraftAppendError](err); ok {
			return result, appendErr
		}
		if isNetworkError(err) {
			return DraftInspectResult{State: DraftStateRemoteUnknown}, &DraftAppendError{
				State: DraftStateRemoteUnknown, Code: "remote_unknown", Err: err}
		}
		return DraftInspectResult{}, err
	}
	return result, nil
}

// FindDraftAppend searches for a draft APPEND by Message-ID and digest in the
// expected mailbox/epoch. Returns a receipt only when exactly one candidate matches.
func (c *Client) FindDraftAppend(ctx context.Context, target DraftTarget, messageID string) (*DraftAppendResult, error) {
	if err := ValidateDraftMailbox(target.Mailbox); err != nil {
		return nil, &DraftAppendError{State: DraftStateRejected, Code: "invalid_mailbox", Err: err}
	}
	var found *DraftAppendResult
	err := c.withConn(ctx, func(conn *imapclient.Client) error {
		if err := ctx.Err(); err != nil {
			return &DraftAppendError{State: DraftStateCancelled, Code: "cancelled", Err: err}
		}
		if !conn.Caps().Has(imap.CapUIDPlus) {
			return &DraftAppendError{State: DraftStateRejected, Code: "uidplus_required",
				Err: errors.New("IMAP server does not advertise UIDPLUS")}
		}
		if err := c.selectMailbox(target.Mailbox); err != nil {
			return err
		}
		if c.selectedUIDValidity != target.UIDValidity {
			return &DraftAppendError{State: DraftStateRejected, Code: "uidvalidity_changed",
				Err: fmt.Errorf("mailbox %q UIDVALIDITY changed: expected %d, got %d",
					target.Mailbox, target.UIDValidity, c.selectedUIDValidity)}
		}
		// Search ALL UIDs in this mailbox.
		if c.selectedNumMessages == 0 {
			return nil
		}
		var allUIDs imap.UIDSet
		allUIDs.AddRange(1, imap.UID(c.selectedNumMessages+1000)) // generous range
		options := &imap.FetchOptions{
			UID:         true,
			Flags:       true,
			BodySection: []*imap.FetchItemBodySection{{Peek: true}},
		}
		cmd := conn.Fetch(allUIDs, options)
		var candidates []DraftAppendResult
		for {
			msg := cmd.Next()
			if msg == nil {
				break
			}
			var rawBuf bytes.Buffer
			var msgFlags []imap.Flag
			msgUID := imap.UID(0)
			for {
				item := msg.Next()
				if item == nil {
					break
				}
				switch v := item.(type) {
				case imapclient.FetchItemDataUID:
					msgUID = v.UID
				case imapclient.FetchItemDataFlags:
					msgFlags = v.Flags
				case imapclient.FetchItemDataBodySection:
					if _, err := io.Copy(&rawBuf, v.Literal); err != nil {
						return fmt.Errorf("read body section: %w", err)
					}
				}
			}
			if msgUID == 0 {
				continue
			}
			// Check \Draft flag.
			hasDraft := false
			for _, f := range msgFlags {
				if f == imap.FlagDraft {
					hasDraft = true
					break
				}
			}
			if !hasDraft {
				continue
			}
			raw := rawBuf.Bytes()
			digest := sha256.Sum256(raw)
			if digest != target.RawSHA256 {
				continue
			}
			// Verify Message-ID.
			parsed, err := mail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				continue
			}
			msgIDHeader := parsed.Header.Get("Message-ID")
			if msgIDHeader == "" {
				continue
			}
			ids, err := parseMessageIDHeader(msgIDHeader)
			if err != nil || len(ids) != 1 {
				continue
			}
			bareExpected, err := normalizeWireMessageID(messageID)
			if err != nil {
				continue
			}
			if ids[0] != bareExpected {
				continue
			}
			candidates = append(candidates, DraftAppendResult{
				State:       DraftStateCreated,
				Code:        "append_uidplus",
				UID:         uint32(msgUID),
				UIDValidity: target.UIDValidity,
			})
		}
		if err := cmd.Close(); err != nil {
			return fmt.Errorf("find draft FETCH: %w", err)
		}
		if len(candidates) == 1 {
			result := candidates[0]
			found = &result
		}
		return nil
	})
	if err != nil {
		if appendErr, ok := errors.AsType[*DraftAppendError](err); ok {
			return nil, appendErr
		}
		if isNetworkError(err) {
			return nil, &DraftAppendError{State: DraftStateRemoteUnknown, Code: "remote_unknown", Err: err}
		}
		return nil, err
	}
	return found, nil
}

// expungeUIDLocked permanently removes a single UID using UID STORE \Deleted +
// UID EXPUNGE. Caller must hold c.mu and have the correct mailbox selected.
func expungeUIDLocked(conn *imapclient.Client, uid uint32) error {
	var uidSet imap.UIDSet
	uidSet.AddNum(imap.UID(uid))
	if err := conn.Store(uidSet, &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagDeleted},
	}, nil).Close(); err != nil {
		return fmt.Errorf("UID STORE \\Deleted: %w", err)
	}
	if err := conn.UIDExpunge(uidSet).Close(); err != nil {
		return fmt.Errorf("UID EXPUNGE: %w", err)
	}
	return nil
}

// draftFetchResult holds the raw data and metadata from a single UID FETCH.
type draftFetchResult struct {
	flags  []string
	digest [32]byte
}

// fetchDraftUID performs a single FETCH for the given UID and returns nil when
// the UID is absent from the mailbox.
func fetchDraftUID(ctx context.Context, conn *imapclient.Client, uid uint32) (*draftFetchResult, error) {
	var uidSet imap.UIDSet
	uidSet.AddNum(imap.UID(uid))
	options := &imap.FetchOptions{
		UID:         true,
		Flags:       true,
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}
	cmd := conn.Fetch(uidSet, options)
	var result *draftFetchResult
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		var rawBuf bytes.Buffer
		var flags []imap.Flag
		for {
			item := msg.Next()
			if item == nil {
				break
			}
			switch v := item.(type) {
			case imapclient.FetchItemDataFlags:
				flags = v.Flags
			case imapclient.FetchItemDataBodySection:
				if _, err := io.Copy(&rawBuf, v.Literal); err != nil {
					return nil, fmt.Errorf("read draft body: %w", err)
				}
			}
		}
		if result == nil {
			r := &draftFetchResult{}
			r.digest = sha256.Sum256(rawBuf.Bytes())
			for _, f := range flags {
				r.flags = append(r.flags, string(f))
			}
			result = r
		}
	}
	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("inspect draft FETCH: %w", err)
	}
	_ = ctx // context checked by caller before withConn
	return result, nil
}
