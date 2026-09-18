package imap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// DraftReceipt is the immutable provider identity recorded by APPEND.
type DraftReceipt struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

// DraftObservation describes one fresh exact-UID check.
type DraftObservation struct {
	State       string         `json:"state"`
	Code        string         `json:"code,omitempty"`
	Mailbox     string         `json:"mailbox"`
	UIDValidity uint32         `json:"uidvalidity"`
	UID         uint32         `json:"uid"`
	Flags       []imaplib.Flag `json:"flags,omitempty"`
	Present     bool           `json:"present"`
	Draft       bool           `json:"draft"`
	Deleted     bool           `json:"deleted"`
	Complete    bool           `json:"complete"`
	UIDPlus     bool           `json:"uidplus"`
	// WriteAttempted records whether RemoveDraft attempted UID STORE, even when
	// a later inspection replaces the provider observation.
	WriteAttempted bool `json:"-"`
}

const draftObservationStateIncomplete = "incomplete"

func validateDraftReceipt(receipt DraftReceipt) error {
	if strings.TrimSpace(receipt.Mailbox) == "" || receipt.UIDValidity == 0 || receipt.UID == 0 {
		return errors.New("invalid IMAP draft receipt")
	}
	return nil
}

func newDraftObservation(receipt DraftReceipt) DraftObservation {
	return DraftObservation{
		State: "unknown", Mailbox: receipt.Mailbox,
		UIDValidity: receipt.UIDValidity, UID: receipt.UID,
	}
}

func (c *Client) selectDraftMailbox(
	conn *imapclient.Client,
	receipt DraftReceipt,
	readOnly, condStore bool,
) (DraftObservation, bool, error) {
	options := &imaplib.SelectOptions{ReadOnly: readOnly, CondStore: condStore}
	selected, err := conn.Select(receipt.Mailbox, options).Wait()
	observation := newDraftObservation(receipt)
	observation.UIDPlus = conn.Caps().Has(imaplib.CapUIDPlus)
	if err != nil {
		observation.State = draftObservationStateIncomplete
		observation.Code = "select_failed"
		return observation, false, fmt.Errorf("SELECT %q: %w", receipt.Mailbox, err)
	}
	c.selectedMailbox = receipt.Mailbox
	c.selectedUIDValidity = selected.UIDValidity
	c.selectedNumMessages = selected.NumMessages
	if selected.UIDValidity != receipt.UIDValidity {
		observation.State = draftObservationStateIncomplete
		observation.Code = "uidvalidity_mismatch"
		return observation, false, fmt.Errorf(
			"UIDVALIDITY mismatch for %q: expected %d, found %d",
			receipt.Mailbox, receipt.UIDValidity, selected.UIDValidity,
		)
	}
	if condStore && selected.HighestModSeq == 0 {
		// The parser discards NOMODSEQ, so zero cannot distinguish it from
		// missing metadata. Refuse both before any draft write.
		observation.State = draftObservationStateIncomplete
		observation.Code = "modseq_unusable"
		return observation, false, errors.New("draft mailbox has no usable HIGHESTMODSEQ")
	}
	return observation, condStore, nil
}

func inspectDraftUID(conn *imapclient.Client, receipt DraftReceipt, observation DraftObservation) (DraftObservation, error) {
	updated, _, err := inspectDraftUIDWithModSeq(conn, receipt, observation, false)
	return updated, err
}

func inspectDraftUIDWithModSeq(
	conn *imapclient.Client,
	receipt DraftReceipt,
	observation DraftObservation,
	includeModSeq bool,
) (DraftObservation, uint64, error) {
	observation.Present = false
	observation.Draft = false
	observation.Deleted = false
	observation.Complete = false
	observation.Code = ""
	observation.Flags = nil
	var uids imaplib.UIDSet
	uids.AddNum(imaplib.UID(receipt.UID))
	messages, err := conn.Fetch(uids, &imaplib.FetchOptions{
		UID: true, Flags: true, ModSeq: includeModSeq,
	}).Collect()
	if err != nil {
		observation.State = draftObservationStateIncomplete
		observation.Code = "fetch_failed"
		return observation, 0, fmt.Errorf("FETCH draft UID %d: %w", receipt.UID, err)
	}
	for _, message := range messages {
		if message == nil || uint32(message.UID) != receipt.UID {
			continue
		}
		observation.Present = true
		observation.State = "present"
		observation.Flags = slices.Clone(message.Flags)
		observation.Draft = hasFlag(message.Flags, imaplib.FlagDraft)
		observation.Deleted = hasFlag(message.Flags, imaplib.FlagDeleted)
		if !observation.Draft {
			observation.Code = "not_draft"
			return observation, message.ModSeq, errors.New("target message is not marked \\Draft")
		}
		if includeModSeq && message.ModSeq == 0 {
			observation.State = draftObservationStateIncomplete
			observation.Code = "modseq_unusable"
			return observation, 0, errors.New("draft target has no usable MODSEQ")
		}
		return observation, message.ModSeq, nil
	}
	observation.State = "absent"
	observation.Code = "absent"
	return observation, 0, nil
}

func hasFlag(flags []imaplib.Flag, want imaplib.Flag) bool {
	for _, flag := range flags {
		if strings.EqualFold(string(flag), string(want)) {
			return true
		}
	}
	return false
}

// InspectDraft performs a fresh read-only SELECT and exact UID FETCH.
func (c *Client) InspectDraft(ctx context.Context, receipt DraftReceipt) (DraftObservation, error) {
	if err := validateDraftReceipt(receipt); err != nil {
		return newDraftObservation(receipt), err
	}
	observation := newDraftObservation(receipt)
	err := c.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		observation.UIDPlus = conn.Caps().Has(imaplib.CapUIDPlus)
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		selectedObservation, useCondStore, selectErr := c.selectDraftMailbox(conn, receipt, true, conn.Caps().Has(imaplib.CapCondStore))
		observation = selectedObservation
		if selectErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && isNetworkError(selectErr) {
				observation.State = draftObservationStateIncomplete
				observation.Code = DraftStateCancelled
				return ctxErr
			}
			return selectErr
		}
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		var inspectErr error
		observation, _, inspectErr = inspectDraftUIDWithModSeq(conn, receipt, observation, useCondStore)
		if inspectErr != nil && ctx.Err() != nil && isNetworkError(inspectErr) {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return ctx.Err()
		}
		return inspectErr
	})
	if err != nil && ctx.Err() != nil && observation.Code == "" {
		observation.State = draftObservationStateIncomplete
		observation.Code = DraftStateCancelled
	}
	return observation, err
}
