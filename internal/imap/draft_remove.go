package imap

import (
	"context"
	"errors"
	"fmt"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// RemoveDraft conditionally marks one exact UID deleted, UID EXPUNGEs it, and
// confirms that the same UID is absent before reporting completion. UID
// EXPUNGE remains a separate, non-conditional step.
func (c *Client) RemoveDraft(ctx context.Context, receipt DraftReceipt) (DraftObservation, error) {
	if err := validateDraftReceipt(receipt); err != nil {
		return newDraftObservation(receipt), err
	}
	observation := newDraftObservation(receipt)
	writeAttempted := false
	err := c.withDraftConn(ctx, func(conn *imapclient.Client) error {
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		if !conn.Caps().Has(imaplib.CapUIDPlus) {
			observation.State = draftObservationStateIncomplete
			observation.Code = "uidplus_required"
			return errors.New("server does not support UIDPLUS; exact draft removal is unavailable")
		}
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		selectedObservation, useCondStore, err := c.selectDraftMailbox(conn, receipt, false, conn.Caps().Has(imaplib.CapCondStore))
		observation = selectedObservation
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && isNetworkError(err) {
				observation.State = draftObservationStateIncomplete
				observation.Code = DraftStateCancelled
				return ctxErr
			}
			return err
		}
		var modSeq uint64
		observation, modSeq, err = inspectDraftUIDWithModSeq(conn, receipt, observation, useCondStore)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && isNetworkError(err) {
				observation.State = draftObservationStateIncomplete
				observation.Code = DraftStateCancelled
				return ctxErr
			}
			return err
		}
		if !observation.Present {
			observation.Code = "not_found"
			return errors.New("draft target is absent")
		}
		if observation.Deleted {
			observation.Code = "already_deleted"
			return errors.New("draft target already has the \\Deleted flag")
		}
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		var uids imaplib.UIDSet
		uids.AddNum(imaplib.UID(receipt.UID))
		storeOptions := (*imaplib.StoreOptions)(nil)
		if useCondStore {
			storeOptions = &imaplib.StoreOptions{UnchangedSince: modSeq}
		}
		writeAttempted = true
		storeCommand := conn.Store(uids, &imaplib.StoreFlags{
			Op: imaplib.StoreFlagsAdd, Silent: true,
			Flags: []imaplib.Flag{imaplib.FlagDeleted},
		}, storeOptions)
		storeErr := storeCommand.Close()
		if storeErr == nil && storeCommand.ModifiedUIDs().Contains(imaplib.UID(receipt.UID)) {
			storeErr = errors.New("draft target changed before UID STORE")
		}
		if storeErr != nil && isNetworkError(storeErr) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				observation.State = draftObservationStateIncomplete
				observation.Code = DraftStateCancelled
				return ctxErr
			}
			observation.State = draftObservationStateIncomplete
			observation.Code = "store_failed"
			return fmt.Errorf("UID STORE \\Deleted: %w", storeErr)
		}
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		selectedObservation, _, err = c.selectDraftMailbox(conn, receipt, false, false)
		observation = selectedObservation
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && isNetworkError(err) {
				observation.State = draftObservationStateIncomplete
				observation.Code = DraftStateCancelled
				return ctxErr
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		observation, err = inspectDraftUID(conn, receipt, observation)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && isNetworkError(err) {
				observation.State = draftObservationStateIncomplete
				observation.Code = DraftStateCancelled
				return ctxErr
			}
			return err
		}
		if storeErr != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = "store_conflict"
			return fmt.Errorf("UID STORE \\Deleted: %w", storeErr)
		}
		if !observation.Present {
			observation.State = draftObservationStateIncomplete
			observation.Code = "store_conflict"
			return errors.New("draft target disappeared after UID STORE")
		}
		if !observation.Draft {
			observation.State = draftObservationStateIncomplete
			observation.Code = "store_conflict"
			return errors.New("draft target lost \\Draft after UID STORE")
		}
		if !observation.Deleted {
			observation.State = draftObservationStateIncomplete
			observation.Code = "store_conflict"
			return errors.New("UID STORE did not set \\Deleted")
		}
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		// ponytail: UID EXPUNGE has no conditional form; fresh checks are the upgrade point.
		if err := conn.UIDExpunge(uids).Close(); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && isNetworkError(err) {
				observation.State = draftObservationStateIncomplete
				observation.Code = DraftStateCancelled
				return ctxErr
			}
			observation.State = draftObservationStateIncomplete
			observation.Code = "expunge_failed"
			return fmt.Errorf("UID EXPUNGE: %w", err)
		}
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		selectedObservation, _, err = c.selectDraftMailbox(conn, receipt, true, false)
		observation = selectedObservation
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && isNetworkError(err) {
				observation.State = draftObservationStateIncomplete
				observation.Code = DraftStateCancelled
				return ctxErr
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			observation.State = draftObservationStateIncomplete
			observation.Code = DraftStateCancelled
			return err
		}
		observation, err = inspectDraftUID(conn, receipt, observation)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil && isNetworkError(err) {
				observation.State = draftObservationStateIncomplete
				observation.Code = DraftStateCancelled
				return ctxErr
			}
			observation.State = draftObservationStateIncomplete
			if observation.Code == "" {
				observation.Code = "confirmation_failed"
			}
			return err
		}
		if observation.Present {
			observation.State = draftObservationStateIncomplete
			observation.Code = "survivor"
			return errors.New("draft target survived UID EXPUNGE")
		}
		observation.State = "absent"
		observation.Code = "removed"
		observation.Complete = true
		return nil
	})
	if err != nil && ctx.Err() != nil && observation.Code == "" {
		observation.State = draftObservationStateIncomplete
		observation.Code = DraftStateCancelled
	}
	observation.WriteAttempted = writeAttempted
	return observation, err
}
