package matrix

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// recoveryCooldown spaces out retries of events whose keys were missing, since
// the backup changes whenever any client uploads a key.
const recoveryCooldown = time.Hour

const recoveryPageSize = 500

// megolmIndex identifies one Megolm message. Each index belongs to one event;
// another event with the same index is a replayed copy.
type megolmIndex struct {
	session id.SessionID
	index   uint
}

type decryptedEvent struct {
	megolmIndex
	store.MatrixEncryptedEvent

	plain *event.Event
}

type rollbackCiphertext struct {
	session id.SessionID
	digest  [sha256.Size]byte
}

func rollbackCiphertextKey(evt *event.Event) (rollbackCiphertext, bool) {
	if evt.Type != event.EventEncrypted || !parseContent(evt) {
		return rollbackCiphertext{}, false
	}
	content := evt.Content.AsEncrypted()
	raw, err := base64.RawStdEncoding.DecodeString(string(content.MegolmCiphertext))
	if err != nil {
		return rollbackCiphertext{}, false
	}
	return rollbackCiphertext{content.SessionID, sha256.Sum256(raw)}, true
}

func (imp *Importer) earlierRollbackCopies(ctx context.Context, sourceID, convID int64, roomID id.RoomID, candidates map[rollbackCiphertext]*event.Event) (map[rollbackCiphertext]*event.Event, error) {
	owners := make(map[rollbackCiphertext]*event.Event)
	var after int64
	for len(candidates) > 0 {
		page, err := imp.store.MatrixDueEncryptedEvents(ctx, convID, after, store.MatrixRecoveryDue{All: true}, recoveryPageSize)
		if err != nil || len(page) == 0 {
			return owners, err
		}
		after = page[len(page)-1].MessageID
		for _, item := range page {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			evt, err := decodeArchivedEvent(item.Ciphertext)
			if err != nil {
				continue
			}
			key, ok := rollbackCiphertextKey(evt)
			candidate := candidates[key]
			if !ok || candidate == nil || compareRelations(evt, candidate) >= 0 {
				continue
			}
			evt.RoomID = roomID
			plain, _, err := imp.runtime.decryptWithIndex(ctx, evt)
			if err != nil {
				return nil, err
			}
			if (plain.Type != event.EventMessage && plain.Type != event.EventSticker) || !parseContent(plain) {
				continue
			}
			// Without a replacement relation, recovery claims the copy as a message.
			if content := plain.Content.AsMessage(); content.RelatesTo.GetReplaceID() != "" {
				_, original, err := imp.editTarget(ctx, sourceID, convID, content.RelatesTo.GetReplaceID())
				if err != nil {
					return nil, err
				}
				if content.NewContent == nil || !editAllowed(original, plain) {
					continue
				}
			}
			if first := owners[key]; first == nil || compareRelations(plain, first) < 0 {
				owners[key] = &event.Event{ID: plain.ID, Timestamp: plain.Timestamp}
			}
		}
	}
	return owners, nil
}

// recoverPlaceholders decrypts archived placeholders with keys from the
// account's key backup, then counts the placeholders that remain.
func (imp *Importer) recoverPlaceholders(ctx context.Context, sourceID int64, opts ImportOptions, sum *ImportSummary) error {
	err := imp.recoverRooms(ctx, sourceID, opts, sum)
	pending, countErr := imp.store.CountMatrixPending(ctx, sourceID, opts.Rooms, opts.ExcludeRooms)
	sum.PendingEncrypted = pending
	return errors.Join(err, countErr)
}

func (imp *Importer) recoverRooms(ctx context.Context, sourceID int64, opts ImportOptions, sum *ImportSummary) error {
	rooms, err := imp.store.MatrixPendingRooms(ctx, sourceID, opts.Rooms, opts.ExcludeRooms, opts.MediaPolicy)
	if err != nil || len(rooms) == 0 || opts.SkipKeyBackup {
		return err
	}
	if imp.runtime.key == nil {
		sum.BackupFailure = imp.runtime.backupKeyError
		return nil
	}
	if err := imp.runtime.prepareBackup(ctx); err != nil {
		sum.BackupFailure = err
		return ctx.Err()
	}
	due := store.MatrixRecoveryDue{All: opts.Full, Stamp: imp.runtime.stamp, RetryBefore: time.Now().Add(-recoveryCooldown).Unix()}
	var failures error
	for _, room := range rooms {
		err := imp.recoverRoom(ctx, sourceID, room, due, opts, sum)
		imp.runtime.releaseRoom(id.RoomID(room.RoomID))
		if imp.runtime.failure != nil {
			sum.BackupFailure = imp.runtime.failure
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return errors.Join(failures, err)
			}
			failures = errors.Join(failures, fmt.Errorf("recover Matrix room %s: %w", room.RoomID, err))
		}
	}
	return errors.Join(failures, ctx.Err())
}

// recoverRoom decrypts a room's due placeholders one page at a time. Each write
// is safe to repeat, so an interrupted page finishes on the next sync.
// Recovered media stays pending; the media retry pass downloads it.
func (imp *Importer) recoverRoom(ctx context.Context, sourceID int64, room store.MatrixPendingRoom, due store.MatrixRecoveryDue, opts ImportOptions, sum *ImportSummary) error {
	pending := pendingMediaOptions(opts)
	pending.MediaMutation = opts.MediaMutation
	conversation := attachmentpolicy.Conversation{Type: room.ConversationType, ParticipantCount: room.ParticipantCount}
	var after int64
	for {
		page, err := imp.store.MatrixDueEncryptedEvents(ctx, room.ConversationID, after, due, recoveryPageSize)
		if err != nil || len(page) == 0 {
			return err
		}
		after = page[len(page)-1].MessageID
		now := time.Now().Unix()
		var decrypted []decryptedEvent
		var failed []store.MatrixEncryptedEvent
		for _, item := range page {
			if err := ctx.Err(); err != nil {
				return err
			}
			evt, err := decodeArchivedEvent(item.Ciphertext)
			if err != nil {
				failed = append(failed, item)
				continue
			}
			evt.RoomID = id.RoomID(room.RoomID)
			plain, index, err := imp.runtime.decryptWithIndex(ctx, evt)
			switch {
			case imp.runtime.failure != nil:
				return err
			case errors.Is(err, errRoomBackupFetch):
				// Leave the page untried so the next sync reads the room again.
				return err
			case err != nil:
				failed = append(failed, item)
			default:
				decrypted = append(decrypted, decryptedEvent{megolmIndex{evt.Content.AsEncrypted().SessionID, index}, item, plain})
			}
		}
		var relations []decryptedEvent
		for _, item := range decrypted {
			settled, relation, err := imp.applyDecrypted(ctx, sourceID, room.ConversationID, item, due.Stamp, conversation, pending, sum)
			if err != nil {
				return err
			}
			if relation {
				relations = append(relations, item)
			} else if !settled {
				failed = append(failed, item.MatrixEncryptedEvent)
			}
		}
		// Apply edits and reactions after their targets, oldest first.
		slices.SortFunc(relations, func(a, b decryptedEvent) int { return compareRelations(a.plain, b.plain) })
		for _, rel := range relations {
			if err := imp.persistEvent(ctx, sourceID, room.ConversationID, rel.plain, conversation, pending, sum); err != nil {
				if !errors.Is(err, errRelationTargetMissing) {
					return err
				}
				sum.RelationsUnresolved++
			}
			if err := imp.store.DeleteMatrixPlaceholder(ctx, sourceID, rel.MessageID); err != nil {
				return err
			}
		}
		if err := imp.store.MarkMatrixEncryptedEventsTried(ctx, failed, due.Stamp, now); err != nil {
			return err
		}
	}
}

// applyDecrypted writes one decrypted placeholder, or reports a relation the
// caller applies after its page's messages. An unsettled placeholder stays pending until the backup changes.
func (imp *Importer) applyDecrypted(ctx context.Context, sourceID, convID int64, item decryptedEvent, stamp string, conversation attachmentpolicy.Conversation, opts ImportOptions, sum *ImportSummary) (settled, relation bool, err error) {
	plain := item.plain
	// The row may have been deleted or reused while the backup was read.
	pending, deleted, err := imp.store.MatrixEncryptedEventPending(ctx, item.MessageID, plain.ID.String())
	if err != nil || !pending {
		return !pending, false, err
	}
	// Copies share plaintext, so only a message's index can move to an earlier
	// copy. Edits and reactions keep their first claim, so none applies twice.
	message := (plain.Type == event.EventMessage || plain.Type == event.EventSticker) && parseContent(plain) && plain.Content.AsMessage().RelatesTo.GetReplaceID() == ""
	if message {
		if err := imp.preferEarlierMessage(ctx, sourceID, convID, item, stamp, conversation, opts); err != nil {
			return false, false, err
		}
	}
	allowed, err := imp.claim(ctx, convID, item.megolmIndex, plain.ID)
	if err != nil || !allowed {
		return false, false, err
	}
	supported := plain.Type == event.EventMessage || plain.Type == event.EventSticker || plain.Type == event.EventReaction
	if !supported || (plain.Type == event.EventReaction && deleted) {
		sum.EventsSkipped++
		return true, false, imp.store.DeleteMatrixPlaceholder(ctx, sourceID, item.MessageID)
	}
	if !parseContent(plain) {
		sum.EventsSkipped++
		return false, false, nil
	}
	if message {
		current, err := imp.archivedEvent(ctx, item.MessageID)
		if err != nil {
			return false, false, err
		}
		// An interrupted recovery may already have written the content and some
		// edits; writing the content again would hide those edits.
		if current.Type == event.EventEncrypted {
			plain.Unsigned.RedactedBecause = nil
			err := imp.persistEvent(ctx, sourceID, convID, plain, conversation, opts, sum)
			switch {
			case errors.Is(err, store.ErrMatrixPlaceholderGone):
				return true, false, nil
			case errors.Is(err, errRelationTargetMissing):
				sum.RelationsUnresolved++
			case err != nil:
				return false, false, err
			}
		} else {
			// Finish the reply link the interrupted run may have missed, without rewriting the body.
			resumed := *plain
			resumed.Mautrix.WasEncrypted = false
			if err := imp.persistEvent(ctx, sourceID, convID, &resumed, conversation, opts, &ImportSummary{}); err != nil {
				return false, false, err
			}
		}
		if err := imp.replayPendingEdits(ctx, sourceID, convID, item.MessageID, conversation, opts, sum); err != nil {
			return false, false, err
		}
		return true, false, imp.store.SetMatrixEncryptedEventRecovered(ctx, item.MessageID, true, "", 0)
	}
	if deleted {
		// A redacted edit or reaction has no effect left to apply.
		return true, false, imp.store.DeleteMatrixPlaceholder(ctx, sourceID, item.MessageID)
	}
	return true, true, nil
}

func (imp *Importer) claim(ctx context.Context, convID int64, key megolmIndex, eventID id.EventID) (bool, error) {
	return imp.store.ClaimMatrixMegolmIndex(ctx, convID, key.session.String(), key.index, eventID.String())
}

// preferEarlierMessage returns a later message that holds item's Megolm index
// to its placeholder, so an older event's text is not credited to its copy.
func (imp *Importer) preferEarlierMessage(ctx context.Context, sourceID, convID int64, item decryptedEvent, stamp string, conversation attachmentpolicy.Conversation, opts ImportOptions) error {
	holder, err := imp.store.MatrixMegolmClaim(ctx, convID, item.session.String(), item.index)
	if err != nil || holder == "" || holder == item.plain.ID.String() {
		return err
	}
	found, err := imp.store.MessageExistsBatchContext(ctx, sourceID, []string{holder})
	if err != nil || found[holder] == 0 {
		return err
	}
	holderID := found[holder]
	ciphertext, err := imp.store.MatrixEncryptedEventCiphertext(ctx, holderID)
	if err != nil || ciphertext == nil {
		return err
	}
	encrypted, err := decodeArchivedEvent(ciphertext)
	if err != nil {
		return err
	}
	// An edit keeps its claim, even against a copy stripped of its outer
	// relation. Copies of a reaction decrypt as reactions and never reach here.
	if !parseContent(encrypted) {
		return nil
	}
	if rel := encrypted.Content.AsEncrypted().RelatesTo; rel != nil && rel.Type == event.RelReplace {
		return nil
	}
	// The ciphertext keeps the holder's ID and timestamp even after an interrupted revert.
	if compareRelations(encrypted, item.plain) <= 0 {
		return nil
	}
	// Reopen the copy first. Each later step can then repeat after an interruption.
	if err := imp.store.SetMatrixEncryptedEventRecovered(ctx, holderID, false, stamp, time.Now().Unix()); err != nil {
		return err
	}
	current, err := imp.archivedEvent(ctx, holderID)
	if err != nil {
		return err
	}
	if current.Type != event.EventEncrypted {
		// Rewrite the row through the same body, raw and index updates.
		encrypted.Mautrix.WasEncrypted = true
		if err := imp.persistEvent(ctx, sourceID, convID, encrypted, conversation, opts, &ImportSummary{}); err != nil {
			return err
		}
	}
	// The copy's media came from the earlier event's content.
	if err := mediaMutation(ctx, opts, func() error {
		return imp.store.ReplaceMessageMatrixAttachments(ctx, holderID, nil)
	}); err != nil {
		return err
	}
	if err := imp.store.SetMessageEditedStateContext(ctx, holderID, false); err != nil {
		return err
	}
	if err := imp.store.ClearMessageRepliesContext(ctx, sourceID, []int64{holderID}); err != nil {
		return err
	}
	metadata, err := imp.mergedMetadata(ctx, holderID, map[string]any{
		editEventKey: nil, editTSKey: nil, "referenced_message_id": nil,
		"matrix_media_content": nil, "matrix_media_content_version": nil,
		"matrix_replay_conflict": item.plain.ID.String(),
	})
	if err != nil {
		return err
	}
	if err := imp.store.SetMessageMetadataContext(ctx, holderID, sql.NullString{String: string(metadata), Valid: true}); err != nil {
		return err
	}
	return imp.store.ReassignMatrixMegolmClaim(ctx, convID, item.session.String(), item.index, item.plain.ID.String())
}
