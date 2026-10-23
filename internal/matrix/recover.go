package matrix

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const recoveryRetryLimit = 5
const recoveryCooldown = time.Hour

type recoveryAttempt struct {
	LastMessageID int64     `json:"last_message_id,omitzero"`
	LastEventID   string    `json:"last_event_id,omitzero"`
	PendingCount  int64     `json:"pending_count,omitzero"`
	Stamp         string    `json:"stamp"`
	TriedAt       time.Time `json:"tried_at,omitzero"`
	NoProgressAt  time.Time `json:"no_progress_at,omitzero"`
}

func (imp *Importer) recoverPlaceholders(ctx context.Context, sourceID int64, state *SyncState, opts ImportOptions, sum *ImportSummary) error {
	sum.PendingEncrypted = 0
	rooms, err := imp.store.MatrixPlaceholderRooms(ctx, sourceID, opts.Rooms, opts.ExcludeRooms)
	if err != nil {
		return err
	}
	if len(rooms) == 0 {
		return nil
	}
	for _, room := range rooms {
		sum.PendingEncrypted += room.Count
	}
	if opts.SkipKeyBackup || imp.runtime.key == nil {
		if !opts.SkipKeyBackup {
			sum.BackupFailure = imp.runtime.backupKeyError
		}
		return nil
	}
	if err := imp.runtime.prepareBackup(ctx); err != nil {
		sum.BackupFailure = err
		return ctx.Err()
	}
	if state.RecoveryRooms == nil {
		state.RecoveryRooms = map[string]recoveryAttempt{}
	}
	slices.SortFunc(rooms, func(a, b store.MatrixPlaceholderRoom) int {
		if order := state.RecoveryRooms[a.RoomID].TriedAt.Compare(state.RecoveryRooms[b.RoomID].TriedAt); order != 0 {
			return order
		}
		return cmp.Compare(a.ConversationID, b.ConversationID)
	})
	retries := 0
	var failures error
	for _, room := range rooms {
		attempt := state.RecoveryRooms[room.RoomID]
		fullRetry := opts.Full || attempt.TriedAt.IsZero() || (attempt.Stamp != imp.runtime.stamp && time.Since(attempt.NoProgressAt) >= recoveryCooldown && retries < recoveryRetryLimit)
		// Rowids can be reused after any hard delete, even with the same count and max ID.
		changed := room.Count != attempt.PendingCount || room.MaxMessageID != attempt.LastMessageID || room.MaxEventID != attempt.LastEventID
		newAfter, hasNew := imp.newAfter[room.RoomID]
		if !changed && !hasNew && !fullRetry {
			continue
		}
		if fullRetry && !opts.Full && !attempt.TriedAt.IsZero() {
			retries++
		}
		after := attempt.LastMessageID
		if fullRetry || (changed && !hasNew) {
			after = 0
		} else if hasNew {
			after = min(after, newAfter)
		}
		if after > 0 && changed {
			previous, err := imp.store.MessageExistsBatchContext(ctx, sourceID, []string{attempt.LastEventID})
			if err != nil {
				return errors.Join(failures, err)
			}
			if previous[attempt.LastEventID] != attempt.LastMessageID {
				after = 0
			}
		}
		retired, err := imp.recoverRoom(ctx, sourceID, room, after, sum)
		attempt.TriedAt = time.Now().UTC()
		imp.runtime.releaseRoom(id.RoomID(room.RoomID))
		if retired > 0 {
			pending, pendingErr := imp.store.MatrixPlaceholderRooms(ctx, sourceID, []string{room.RoomID}, nil)
			if pendingErr != nil {
				return errors.Join(failures, err, pendingErr)
			}
			// SQLite can reuse retired rowids, so only pending rows guard future work.
			room.Count, room.MaxMessageID, room.MaxEventID = 0, 0, ""
			if len(pending) > 0 {
				room = pending[0]
			}
		}
		attempt.LastMessageID = room.MaxMessageID
		attempt.LastEventID = room.MaxEventID
		attempt.PendingCount = room.Count
		if err != nil || imp.runtime.failure != nil {
			attempt.Stamp = ""
			attempt.NoProgressAt = time.Time{}
		}
		if err != nil {
			state.RecoveryRooms[room.RoomID] = attempt
			if ctx.Err() != nil {
				return errors.Join(failures, err)
			}
			failures = errors.Join(failures, fmt.Errorf("recover Matrix room %s: %w", room.RoomID, err))
			continue
		}
		if imp.runtime.failure != nil {
			state.RecoveryRooms[room.RoomID] = attempt
			sum.BackupFailure = imp.runtime.failure
			break
		}
		if fullRetry {
			attempt.Stamp = imp.runtime.stamp
			if retired == 0 {
				attempt.NoProgressAt = attempt.TriedAt
			} else {
				attempt.NoProgressAt = time.Time{}
			}
		}
		state.RecoveryRooms[room.RoomID] = attempt
		delete(imp.newAfter, room.RoomID)
	}
	return errors.Join(failures, ctx.Err())
}

func (imp *Importer) recoverRoom(ctx context.Context, sourceID int64, room store.MatrixPlaceholderRoom, after int64, sum *ImportSummary) (int64, error) {
	type relation struct {
		messageID int64
		evt       *event.Event
	}
	var retired int64
	var claims map[string]string
	for {
		items, scanned, err := imp.store.MatrixPlaceholders(ctx, room.ConversationID, after, 500)
		if err != nil {
			return retired, err
		}
		if scanned == after {
			break
		}
		after = scanned
		events := make(map[int64]*event.Event, len(items))
		for _, item := range items {
			evt, err := decodeArchivedEvent(item.Raw)
			if err != nil {
				slog.Warn("skip undecodable Matrix placeholder", "message_id", item.MessageID, "error", err)
				continue
			}
			if evt.Type != event.EventEncrypted {
				continue
			}
			events[item.MessageID] = evt
		}
		if len(events) == 0 {
			continue
		}
		replayKeys := make(map[int64]string, len(events))
		decrypted := make(map[int64]*event.Event, len(events))
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return retired, err
			}
			evt := events[item.MessageID]
			if evt == nil {
				continue
			}
			evt.RoomID = id.RoomID(room.RoomID)
			plain, index, err := imp.runtime.decryptWithIndex(ctx, evt)
			if err != nil {
				if imp.runtime.failure != nil {
					break
				}
				if errors.Is(err, errRoomBackupFetch) {
					return retired, err
				}
				continue
			}
			decrypted[item.MessageID] = plain
			replayKeys[item.MessageID] = fmt.Sprintf("matrix_megolm_%s_%d", evt.Content.AsEncrypted().SessionID, index)
		}
		if len(decrypted) == 0 {
			if imp.runtime.failure != nil {
				return retired, ctx.Err()
			}
			continue
		}
		counters := *sum
		pageRetired := int64(0)
		err = imp.store.MatrixRecoveryTransaction(ctx, func(s *store.Store) error {
			worker := NewImporter(s, imp.runtime)
			worker.newAfter = imp.newAfter
			retire := func(messageID int64, plain *event.Event, target id.EventID) error {
				found, err := s.MessageExistsBatchContext(ctx, sourceID, []string{target.String()})
				if err != nil {
					return err
				}
				targetID := found[target.String()]
				if targetID != 0 && targetID != messageID {
					targetMessage, err := s.GetMessageRelationTarget(targetID)
					if err != nil {
						return err
					}
					if targetMessage.ConversationID != room.ConversationID {
						targetID = 0
					}
				}
				claimID := targetID
				if claimID == 0 {
					claimID = messageID
				}
				if _, err := worker.claimRecoveredEvent(ctx, room.ConversationID, claimID, replayKeys[messageID], plain.ID.String(), &claims); err != nil {
					return err
				}
				if targetID == messageID {
					return nil
				}
				if targetID != 0 {
					return s.DeleteMatrixPlaceholder(ctx, sourceID, messageID)
				}
				metadata, err := worker.mergedMetadata(messageID, map[string]any{"matrix_event_type": plain.Type.Type})
				if err != nil {
					return err
				}
				if err := s.SetMessageMetadataContext(ctx, messageID, sql.NullString{String: string(metadata), Valid: true}); err != nil {
					return err
				}
				return s.HideMatrixPlaceholder(ctx, sourceID, messageID)
			}
			var relations []relation
			for _, item := range items {
				if err := ctx.Err(); err != nil {
					return err
				}
				plain := decrypted[item.MessageID]
				if plain == nil {
					continue
				}
				replayKey := replayKeys[item.MessageID]
				matches, err := s.MatrixPlaceholderMatches(ctx, sourceID, room.ConversationID, item, plain.ID.String())
				if err != nil {
					return err
				}
				if !matches {
					continue
				}
				allowed, err := worker.claimRecoveredEvent(ctx, room.ConversationID, 0, replayKey, plain.ID.String(), &claims)
				if err != nil {
					return err
				}
				if !allowed {
					continue
				}
				if (plain.Type != event.EventMessage && plain.Type != event.EventSticker && plain.Type != event.EventReaction) || (plain.Type == event.EventReaction && item.Deleted) {
					if err := retire(item.MessageID, plain, ""); err != nil {
						return err
					}
					counters.EventsSkipped++
					counters.PendingEncrypted--
					pageRetired++
					continue
				}
				if !parseContent(plain) {
					counters.EventsSkipped++
					continue
				}
				if (plain.Type == event.EventMessage || plain.Type == event.EventSticker) && plain.Content.AsMessage().RelatesTo.GetReplaceID() == "" {
					if err := retire(item.MessageID, plain, plain.ID); err != nil {
						return err
					}
					plain.Unsigned.RedactedBecause = nil
					if err := worker.persistEvent(ctx, sourceID, room.ConversationID, plain, &counters); err != nil {
						if !errors.Is(err, errRelationTargetMissing) {
							return err
						}
						counters.RelationsUnresolved++
					}
					if err := worker.replayPendingEdits(ctx, sourceID, room.ConversationID, item.MessageID, &counters); err != nil {
						return err
					}
					counters.PendingEncrypted--
					pageRetired++
					continue
				}
				if shouldDeferRelation(plain) && !item.Deleted {
					relations = append(relations, relation{item.MessageID, plain})
					continue
				}
				if err := retire(item.MessageID, plain, plain.Content.AsMessage().RelatesTo.GetReplaceID()); err != nil {
					return err
				}
				counters.PendingEncrypted--
				pageRetired++
			}
			slices.SortFunc(relations, func(a, b relation) int { return compareRelations(a.evt, b.evt) })
			for _, rel := range relations {
				allowed, err := worker.claimRecoveredEvent(ctx, room.ConversationID, 0, replayKeys[rel.messageID], rel.evt.ID.String(), &claims)
				if err != nil {
					return err
				}
				if !allowed {
					continue
				}
				err = worker.persistEvent(ctx, sourceID, room.ConversationID, rel.evt, &counters)
				if errors.Is(err, errRelationTargetMissing) {
					counters.RelationsUnresolved++
				} else if err != nil {
					return err
				}
				target := rel.evt.Content.AsMessage().RelatesTo.GetReplaceID()
				if rel.evt.Type == event.EventReaction {
					target = rel.evt.Content.AsReaction().RelatesTo.EventID
				}
				if err := retire(rel.messageID, rel.evt, target); err != nil {
					return err
				}
				counters.PendingEncrypted--
				pageRetired++
			}
			return nil
		})
		if err != nil {
			return retired, err
		}
		*sum = counters
		retired += pageRetired
		if imp.runtime.failure != nil {
			return retired, ctx.Err()
		}
	}

	return retired, nil
}

// A zero messageID checks the claim before applying content; a row ID also saves it.
func (imp *Importer) claimRecoveredEvent(ctx context.Context, conversationID, messageID int64, key, eventID string, claims *map[string]string) (bool, error) {
	if *claims == nil {
		var err error
		*claims, err = imp.store.MatrixRecoveredEvents(ctx, conversationID)
		if err != nil {
			return false, err
		}
	}
	if previous := (*claims)[key]; previous != "" && previous != eventID {
		return false, nil
	}
	if messageID != 0 {
		metadata, err := imp.mergedMetadata(messageID, map[string]any{key: eventID})
		if err != nil {
			return false, err
		}
		if err := imp.store.SetMessageMetadataContext(ctx, messageID, sql.NullString{String: string(metadata), Valid: true}); err != nil {
			return false, err
		}
		(*claims)[key] = eventID
	}
	return true, nil
}
