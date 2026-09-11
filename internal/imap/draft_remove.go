package imap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

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
			return &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: err}
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
			return &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: err}
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
		if !conn.Caps().Has(imap.CapCondStore) {
			return &DraftAppendError{State: DraftStateRejected, Code: "conditional_store_required",
				Err: errors.New("IMAP server does not advertise CONDSTORE")}
		}
		if err := ctx.Err(); err != nil {
			return &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: err}
		}
		if fetchResult.modSeq == 0 {
			return &DraftAppendError{State: DraftStateRejected, Code: "conditional_store_required",
				Err: errors.New("IMAP FETCH returned no MODSEQ for conditional removal")}
		}
		if err := conditionalExpungeUIDLocked(ctx, conn, target.UID, fetchResult.modSeq); err != nil {
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

func conditionalExpungeUIDLocked(ctx context.Context, conn *imapclient.Client, uid uint32, modSeq uint64) error {
	if err := ctx.Err(); err != nil {
		return &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: err}
	}
	var uidSet imap.UIDSet
	uidSet.AddNum(imap.UID(uid))
	responses, err := conn.Store(uidSet, &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: false,
		Flags:  []imap.Flag{imap.FlagDeleted},
	}, &imap.StoreOptions{UnchangedSince: modSeq}).Collect()
	if err != nil {
		return fmt.Errorf("conditional UID STORE \\Deleted: %w", err)
	}
	if len(responses) == 0 {
		return &DraftAppendError{State: DraftRemoteChanged, Code: "remote_changed",
			Err: errors.New("IMAP conditional STORE found a changed message")}
	}
	if err := ctx.Err(); err != nil {
		return &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: err}
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
	modSeq uint64
}

// fetchDraftUID performs a single FETCH for the given UID and returns nil when
// the UID is absent from the mailbox.
func fetchDraftUID(ctx context.Context, conn *imapclient.Client, uid uint32) (*draftFetchResult, error) {
	var uidSet imap.UIDSet
	uidSet.AddNum(imap.UID(uid))
	options := &imap.FetchOptions{
		UID:         true,
		Flags:       true,
		ModSeq:      conn.Caps().Has(imap.CapCondStore),
		BodySection: []*imap.FetchItemBodySection{{Peek: true}},
	}
	cmd := conn.Fetch(uidSet, options)
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	var result *draftFetchResult
	for {
		msg := cmd.Next()
		if msg == nil {
			break
		}
		var rawBuf bytes.Buffer
		var flags []imap.Flag
		var modSeq uint64
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
					if ctxErr := ctx.Err(); ctxErr != nil {
						return nil, &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: ctxErr}
					}
					return nil, fmt.Errorf("read draft body: %w", err)
				}
			case imapclient.FetchItemDataModSeq:
				modSeq = v.ModSeq
			}
		}
		if result == nil {
			r := &draftFetchResult{}
			r.digest = sha256.Sum256(rawBuf.Bytes())
			for _, f := range flags {
				r.flags = append(r.flags, string(f))
			}
			r.modSeq = modSeq
			result = r
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: err}
	}
	if err := cmd.Close(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: ctxErr}
		}
		return nil, fmt.Errorf("inspect draft FETCH: %w", err)
	}
	return result, nil
}
