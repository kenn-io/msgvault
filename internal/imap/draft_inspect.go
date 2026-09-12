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

// InspectDraft fetches the remote draft and classifies its state. It never
// changes local or server state.
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
		if err := c.selectMailboxContext(ctx, conn, target.Mailbox); err != nil {
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
		for _, flag := range fetchResult.flags {
			if imap.Flag(flag) == imap.FlagDraft {
				if fetchResult.digest == target.RawSHA256 {
					result.State = DraftRemotePresent
				} else {
					result.State = DraftRemoteChanged
				}
				return nil
			}
		}
		result.State = DraftRemoteFlagMissing
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

func (c *Client) selectMailboxContext(ctx context.Context, conn *imapclient.Client, mailbox string) error {
	if c.selectedMailbox == mailbox {
		return nil
	}
	result := make(chan struct {
		data *imap.SelectData
		err  error
	}, 1)
	go func() {
		data, err := conn.Select(mailbox, nil).Wait()
		result <- struct {
			data *imap.SelectData
			err  error
		}{data: data, err: err}
	}()
	select {
	case selection := <-result:
		if err := ctx.Err(); err != nil {
			return &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: err}
		}
		if selection.err != nil {
			return fmt.Errorf("SELECT %q: %w", mailbox, selection.err)
		}
		c.selectedMailbox = mailbox
		c.selectedUIDValidity = selection.data.UIDValidity
		c.selectedNumMessages = selection.data.NumMessages
		return nil
	case <-ctx.Done():
		_ = conn.Close()
		return &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: ctx.Err()}
	}
}

type draftFetchResult struct {
	flags  []string
	digest [32]byte
	modSeq uint64
}

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
			switch value := item.(type) {
			case imapclient.FetchItemDataFlags:
				flags = value.Flags
			case imapclient.FetchItemDataBodySection:
				if _, err := io.Copy(&rawBuf, value.Literal); err != nil {
					if ctxErr := ctx.Err(); ctxErr != nil {
						return nil, &DraftAppendError{State: DraftStateCancelled, Code: DraftStateCancelled, Err: ctxErr}
					}
					return nil, fmt.Errorf("read draft body: %w", err)
				}
			case imapclient.FetchItemDataModSeq:
				modSeq = value.ModSeq
			}
		}
		if result == nil {
			result = &draftFetchResult{digest: sha256.Sum256(rawBuf.Bytes()), modSeq: modSeq}
			for _, flag := range flags {
				result.flags = append(result.flags, string(flag))
			}
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
