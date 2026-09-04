package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// IMAPMembershipObservation records one observed mailbox UID and its message identity.
type IMAPMembershipObservation struct {
	Mailbox                  string
	UIDValidity              uint32
	UID                      uint32
	SourceMessageID          string
	CanonicalSourceMessageID string
	RFC822MessageID          string
	RawSHA256                [32]byte
	RawSize                  int64
	Flags                    []string
}

// IMAPMailboxDelta is the durable state change collected for one mailbox.
type IMAPMailboxDelta struct {
	Mailbox      string
	State        IMAPFolderState
	Memberships  []IMAPMembershipObservation
	VanishedUIDs []uint32
	Reset        bool
}

type normalizedIMAPMailboxDelta struct {
	delta       IMAPMailboxDelta
	mailbox     string
	uidValidity uint32
}

// GetIMAPKnownUIDs returns the saved UIDs for each mailbox in a source.
func (s *Store) GetIMAPKnownUIDs(sourceID int64) (map[string][]uint32, error) {
	rows, err := s.db.Query(`
		SELECT state.mailbox, membership.uid
		FROM imap_folder_state state
		LEFT JOIN imap_message_memberships membership
		  ON membership.source_id = state.source_id
		 AND membership.mailbox = state.mailbox
		 AND membership.uidvalidity = state.uidvalidity
		WHERE state.source_id = ?
		ORDER BY state.mailbox, membership.uid
	`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("query known IMAP UIDs for source %d: %w", sourceID, err)
	}
	defer func() { _ = rows.Close() }()

	known := make(map[string][]uint32)
	for rows.Next() {
		var mailbox string
		var uid sql.Null[uint32]
		if err := rows.Scan(&mailbox, &uid); err != nil {
			return nil, fmt.Errorf("scan known IMAP UID: %w", err)
		}
		if _, ok := known[mailbox]; !ok {
			known[mailbox] = make([]uint32, 0)
		}
		if uid.Valid {
			known[mailbox] = append(known[mailbox], uid.V)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate known IMAP UIDs: %w", err)
	}
	return known, nil
}

// aliasUIDChunkSize bounds one alias query's bound parameters, well under
// SQLite's per-statement limit.
const aliasUIDChunkSize = 900

// GetIMAPSourceMessageAliases returns the canonical archived source identity
// for the named mailbox UIDs that are in the current saved epoch. Callers ask
// for the UIDs one run actually fetches: resolving the whole source instead
// reads every stored membership to use a handful of them.
func (s *Store) GetIMAPSourceMessageAliases(
	sourceID int64,
	mailbox string,
	uids []uint32,
) (map[string]string, error) {
	aliases := make(map[string]string, len(uids))
	for chunk := range slices.Chunk(uids, aliasUIDChunkSize) {
		if err := s.readIMAPMessageAliases(sourceID, mailbox, chunk, aliases); err != nil {
			return nil, err
		}
	}
	return aliases, nil
}

// readIMAPMessageAliases adds one chunk of UIDs to aliases. It is a separate
// function so each chunk closes its rows before the next query runs.
func (s *Store) readIMAPMessageAliases(
	sourceID int64,
	mailbox string,
	uids []uint32,
	aliases map[string]string,
) error {
	if len(uids) == 0 {
		return nil
	}
	args := make([]any, 0, len(uids)+2)
	args = append(args, sourceID, mailbox)
	for _, uid := range uids {
		args = append(args, uid)
	}
	rows, err := s.db.Query(`
		SELECT membership.uid, messages.source_message_id
		FROM imap_message_memberships membership
		JOIN imap_folder_state state
		  ON state.source_id = membership.source_id
		 AND state.mailbox = membership.mailbox
		 AND state.uidvalidity = membership.uidvalidity
		JOIN messages ON messages.id = membership.message_id
		WHERE membership.source_id = ? AND membership.mailbox = ?
		  AND messages.source_message_id IS NOT NULL
		  AND membership.uid IN (?`+strings.Repeat(",?", len(uids)-1)+`)
	`, args...)
	if err != nil {
		return fmt.Errorf("query IMAP source message aliases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var canonicalSourceMessageID string
		var uid uint32
		if err := rows.Scan(&uid, &canonicalSourceMessageID); err != nil {
			return fmt.Errorf("scan IMAP source message alias: %w", err)
		}
		aliases[fmt.Sprintf("%s|%d", mailbox, uid)] = canonicalSourceMessageID
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate IMAP source message aliases: %w", err)
	}
	return nil
}

// ApplyIMAPMailboxDeltas atomically replaces the source's authoritative IMAP
// mailbox topology, memberships, labels, tombstones, and cursors. Callers must
// pass one delta for every current mailbox; a non-nil empty slice means the
// authoritative current topology is empty and retires every saved mailbox.
func (s *Store) ApplyIMAPMailboxDeltas(sourceID int64, deltas []IMAPMailboxDelta) error {
	return s.applyIMAPMailboxDeltas(context.Background(), sourceID, 0, deltas)
}

// ApplyIMAPMailboxDeltasForSyncContext applies an authoritative topology only
// if syncRunID is still the latest completed generation for sourceID. The
// generation check and topology replacement share one transaction and source
// lock, so a newer StartSync cannot interleave between them.
func (s *Store) ApplyIMAPMailboxDeltasForSyncContext(
	ctx context.Context,
	sourceID int64,
	syncRunID int64,
	deltas []IMAPMailboxDelta,
) error {
	if syncRunID <= 0 {
		return fmt.Errorf("apply IMAP mailbox deltas: invalid sync generation %d", syncRunID)
	}
	return s.applyIMAPMailboxDeltas(ctx, sourceID, syncRunID, deltas)
}

func (s *Store) applyIMAPMailboxDeltas(
	ctx context.Context,
	sourceID int64,
	syncRunID int64,
	deltas []IMAPMailboxDelta,
) error {
	if deltas == nil {
		return errors.New("apply IMAP mailbox deltas: nil authoritative topology")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if syncRunID > 0 {
			if err := validateCurrentSyncGeneration(
				ctx, tx, sourceID, syncRunID, SyncStatusCompleted,
			); err != nil {
				return err
			}
		}
		normalizedDeltas, err := normalizeIMAPMailboxDeltas(deltas)
		if err != nil {
			return err
		}
		resolver := imapMembershipResolver{tx: tx, sourceID: sourceID}
		if err := resolver.primeRawIdentities(normalizedDeltas); err != nil {
			return err
		}

		affected := make(map[int64]struct{})
		if err := captureUntrackedIMAPMessageIDs(tx, sourceID, affected); err != nil {
			return err
		}
		currentMailboxes := make(map[string]struct{}, len(normalizedDeltas))
		for _, normalized := range normalizedDeltas {
			currentMailboxes[normalized.mailbox] = struct{}{}
		}
		if err := retireAbsentIMAPMailboxes(
			tx, sourceID, currentMailboxes, affected,
		); err != nil {
			return err
		}
		for _, normalized := range normalizedDeltas {
			delta := normalized.delta
			// A Reset delta is authoritative for the whole mailbox, but almost
			// every row it republishes is identical to the saved one. Read the
			// saved rows once and diff against them, so only real changes are
			// written and only their messages have labels rebuilt below.
			var stored map[imapMembershipUID]storedIMAPMembership
			if delta.Reset {
				loaded, err := loadIMAPMailboxMemberships(tx, sourceID, normalized.mailbox)
				if err != nil {
					return fmt.Errorf("load memberships for mailbox %q: %w", normalized.mailbox, err)
				}
				stored = loaded
				observed := make(map[imapMembershipUID]struct{}, len(delta.Memberships))
				for _, observation := range delta.Memberships {
					observed[imapMembershipUID{
						uidValidity: normalized.uidValidity, uid: observation.UID,
					}] = struct{}{}
				}
				// Whatever the reset does not republish is gone from the mailbox.
				// This runs before any insert, as the wholesale delete did.
				if err := deleteUnobservedIMAPMemberships(
					tx, sourceID, normalized.mailbox, stored, observed, affected,
				); err != nil {
					return err
				}
			}

			for _, uid := range delta.VanishedUIDs {
				if err := captureIMAPMembershipMessageIDs(
					tx, affected,
					`SELECT message_id FROM imap_message_memberships
					 WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`,
					sourceID, normalized.mailbox, normalized.uidValidity, uid,
				); err != nil {
					return fmt.Errorf("capture vanished UID %d in mailbox %q: %w", uid, normalized.mailbox, err)
				}
				if _, err := tx.Exec(`
					DELETE FROM imap_message_memberships
					WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
				`, sourceID, normalized.mailbox, normalized.uidValidity, uid); err != nil {
					return fmt.Errorf("remove vanished UID %d in mailbox %q: %w", uid, normalized.mailbox, err)
				}
				delete(stored, imapMembershipUID{uidValidity: normalized.uidValidity, uid: uid})
			}

			for _, observation := range delta.Memberships {
				observation.Mailbox = normalized.mailbox
				observation.UIDValidity = normalized.uidValidity
				messageID, err := resolver.resolve(observation)
				if err != nil {
					return err
				}
				flags := observation.Flags
				if flags == nil {
					flags = []string{}
				}
				if delta.Reset {
					key := imapMembershipUID{
						uidValidity: observation.UIDValidity, uid: observation.UID,
					}
					prior, saved := stored[key]
					delete(stored, key)
					if saved && prior.flagsDecoded &&
						prior.messageID == messageID && slices.Equal(prior.flags, flags) {
						// Identical membership: writing it would only move
						// updated_at and rebuild labels that cannot have changed.
						continue
					}
				}
				if observation.SourceMessageID != "" {
					if err := captureIMAPMembershipMessageIDs(
						tx, affected,
						`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`,
						sourceID, observation.SourceMessageID,
					); err != nil {
						return fmt.Errorf("capture observed IMAP message: %w", err)
					}
				}
				affected[messageID] = struct{}{}
				if err := captureIMAPMembershipMessageIDs(
					tx, affected,
					`SELECT message_id FROM imap_message_memberships
					 WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`,
					sourceID, observation.Mailbox, observation.UIDValidity, observation.UID,
				); err != nil {
					return fmt.Errorf("capture replaced IMAP membership: %w", err)
				}

				flagsJSON, err := json.Marshal(flags)
				if err != nil {
					return fmt.Errorf("marshal IMAP flags: %w", err)
				}
				if _, err := tx.Exec(fmt.Sprintf(`
					INSERT INTO imap_message_memberships
						(source_id, mailbox, uidvalidity, uid, message_id, flags, updated_at)
					VALUES (?, ?, ?, ?, ?, %s, %s)
					ON CONFLICT(source_id, mailbox, uidvalidity, uid) DO UPDATE SET
						message_id = excluded.message_id,
						flags = excluded.flags,
						updated_at = %s
				`, s.dialect.JSONBindExpr(), s.dialect.Now(), s.dialect.Now()),
					sourceID, observation.Mailbox, observation.UIDValidity,
					observation.UID, messageID, string(flagsJSON),
				); err != nil {
					return fmt.Errorf("upsert IMAP membership for mailbox %q UID %d: %w",
						observation.Mailbox, observation.UID, err)
				}
			}
		}

		for _, messageID := range sortedIMAPMessageIDs(affected) {
			mailboxes, err := imapMembershipMailboxes(tx, sourceID, messageID)
			if err != nil {
				return err
			}
			labelIDs := make([]int64, 0, len(mailboxes))
			for _, mailbox := range mailboxes {
				labelID, err := ensureIMAPMailboxLabel(tx, sourceID, mailbox)
				if err != nil {
					return err
				}
				labelIDs = append(labelIDs, labelID)
			}
			if err := replaceMessageLabelsTx(tx, messageID, labelIDs); err != nil {
				return fmt.Errorf("replace labels for IMAP message %d: %w", messageID, err)
			}
			if len(mailboxes) == 0 {
				if _, err := tx.Exec(fmt.Sprintf(`
					UPDATE messages SET deleted_from_source_at = %s
					WHERE id = ? AND source_id = ? AND deleted_from_source_at IS NULL
				`, s.dialect.Now()), messageID, sourceID); err != nil {
					return fmt.Errorf("tombstone IMAP message %d: %w", messageID, err)
				}
			} else if _, err := tx.Exec(`
				UPDATE messages SET deleted_from_source_at = NULL
				WHERE id = ? AND source_id = ? AND deleted_from_source_at IS NOT NULL
			`, messageID, sourceID); err != nil {
				return fmt.Errorf("clear IMAP tombstone for message %d: %w", messageID, err)
			}
		}

		for _, normalized := range normalizedDeltas {
			delta := normalized.delta
			if _, err := tx.Exec(fmt.Sprintf(`
				INSERT INTO imap_folder_state
					(source_id, mailbox, uidvalidity, uidnext, highest_modseq, updated_at)
				VALUES (?, ?, ?, ?, ?, %s)
				ON CONFLICT(source_id, mailbox) DO UPDATE SET
					uidvalidity = excluded.uidvalidity,
					uidnext = excluded.uidnext,
					highest_modseq = excluded.highest_modseq,
					updated_at = %s
			`, s.dialect.Now(), s.dialect.Now()),
				sourceID, normalized.mailbox, normalized.uidValidity, delta.State.UIDNext,
				strconv.FormatUint(delta.State.HighestModSeq, 10),
			); err != nil {
				return fmt.Errorf("upsert IMAP folder state for %q: %w", normalized.mailbox, err)
			}
		}
		return nil
	})
}

// imapMembershipUID identifies one saved membership row within a mailbox.
type imapMembershipUID struct {
	uidValidity uint32
	uid         uint32
}

type storedIMAPMembership struct {
	messageID int64
	flags     []string
	// flagsDecoded is false when the saved flags JSON did not parse. Such a row
	// cannot be compared, so it is always rewritten.
	flagsDecoded bool
}

// loadIMAPMailboxMemberships reads every saved membership of one mailbox so a
// Reset delta can diff against it. Flags are decoded rather than compared as
// stored text: SQLite returns the JSON we wrote, PostgreSQL reformats it.
func loadIMAPMailboxMemberships(
	tx *loggedTx, sourceID int64, mailbox string,
) (map[imapMembershipUID]storedIMAPMembership, error) {
	rows, err := tx.Query(`
		SELECT uidvalidity, uid, message_id, flags
		FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ?
	`, sourceID, mailbox)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	stored := make(map[imapMembershipUID]storedIMAPMembership)
	for rows.Next() {
		var (
			key       imapMembershipUID
			messageID int64
			flagsJSON sql.NullString
		)
		if err := rows.Scan(&key.uidValidity, &key.uid, &messageID, &flagsJSON); err != nil {
			return nil, err
		}
		saved := storedIMAPMembership{messageID: messageID, flags: []string{}, flagsDecoded: true}
		if flagsJSON.Valid && flagsJSON.String != "" {
			if err := json.Unmarshal([]byte(flagsJSON.String), &saved.flags); err != nil {
				saved.flags, saved.flagsDecoded = nil, false
			}
		}
		stored[key] = saved
	}
	return stored, rows.Err()
}

// deleteUnobservedIMAPMemberships removes the saved rows of a mailbox that a
// Reset delta does not republish, drops them from stored, and marks their
// messages for label reconciliation.
func deleteUnobservedIMAPMemberships(
	tx *loggedTx,
	sourceID int64,
	mailbox string,
	stored map[imapMembershipUID]storedIMAPMembership,
	observed map[imapMembershipUID]struct{},
	affected map[int64]struct{},
) error {
	keys := make([]imapMembershipUID, 0, len(stored))
	for key, prior := range stored {
		if _, kept := observed[key]; kept {
			continue
		}
		keys = append(keys, key)
		affected[prior.messageID] = struct{}{}
		delete(stored, key)
	}
	if len(keys) == 0 {
		return nil
	}
	if len(stored) == 0 {
		// Nothing survived: an emptied mailbox, or a new UIDVALIDITY epoch.
		// One statement instead of one per row.
		if _, err := tx.Exec(`
			DELETE FROM imap_message_memberships
			WHERE source_id = ? AND mailbox = ?
		`, sourceID, mailbox); err != nil {
			return fmt.Errorf("reset IMAP memberships for mailbox %q: %w", mailbox, err)
		}
		return nil
	}
	slices.SortFunc(keys, func(a, b imapMembershipUID) int {
		if a.uidValidity != b.uidValidity {
			return cmp.Compare(a.uidValidity, b.uidValidity)
		}
		return cmp.Compare(a.uid, b.uid)
	})
	for _, key := range keys {
		if _, err := tx.Exec(`
			DELETE FROM imap_message_memberships
			WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
		`, sourceID, mailbox, key.uidValidity, key.uid); err != nil {
			return fmt.Errorf("remove absent UID %d in mailbox %q: %w", key.uid, mailbox, err)
		}
	}
	return nil
}

func captureUntrackedIMAPMessageIDs(
	tx *loggedTx, sourceID int64, affected map[int64]struct{},
) error {
	// Partial, filtered, interrupted, or failed runs can import a live message
	// without publishing its membership. Every later authoritative apply must
	// reconcile such rows even after the initial membership baseline exists.
	if err := captureIMAPMembershipMessageIDs(tx, affected, `
		SELECT messages.id
		FROM messages
		WHERE messages.source_id = ?
		  AND messages.deleted_from_source_at IS NULL
		  AND NOT EXISTS (
			SELECT 1 FROM imap_message_memberships membership
			WHERE membership.source_id = messages.source_id
			  AND membership.message_id = messages.id
		  )
	`, sourceID); err != nil {
		return fmt.Errorf("capture live messages without IMAP membership: %w", err)
	}

	var membershipCount int64
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?
	`, sourceID).Scan(&membershipCount); err != nil {
		return fmt.Errorf("count prior IMAP memberships: %w", err)
	}
	if membershipCount != 0 {
		return nil
	}

	// The membership table is introduced after messages and mailbox labels may
	// already exist. Live rows were captured above; on the first authoritative
	// baseline also include tombstoned rows that still carry legacy labels. Rows
	// already both tombstoned and label-free were reconciled by an earlier empty
	// baseline, so later empty-mailbox syncs stay proportional to live state.
	if err := captureIMAPMembershipMessageIDs(
		tx, affected, `
			SELECT messages.id
			FROM message_labels
			JOIN messages ON messages.id = message_labels.message_id
			WHERE messages.source_id = ?
			  AND messages.deleted_from_source_at IS NOT NULL
		`, sourceID,
	); err != nil {
		return fmt.Errorf("capture initial IMAP baseline messages: %w", err)
	}
	return nil
}

func retireAbsentIMAPMailboxes(
	tx *loggedTx,
	sourceID int64,
	current map[string]struct{},
	affected map[int64]struct{},
) error {
	rows, err := tx.Query(`
		SELECT mailbox FROM imap_folder_state WHERE source_id = ?
		UNION
		SELECT mailbox FROM imap_message_memberships WHERE source_id = ?
		ORDER BY mailbox
	`, sourceID, sourceID)
	if err != nil {
		return fmt.Errorf("query prior IMAP mailbox topology: %w", err)
	}
	var retired []string
	for rows.Next() {
		var mailbox string
		if err := rows.Scan(&mailbox); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan prior IMAP mailbox topology: %w", err)
		}
		if _, ok := current[mailbox]; !ok {
			retired = append(retired, mailbox)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate prior IMAP mailbox topology: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close prior IMAP mailbox topology: %w", err)
	}

	for _, mailbox := range retired {
		if err := captureIMAPMembershipMessageIDs(
			tx, affected,
			`SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ?`,
			sourceID, mailbox,
		); err != nil {
			return fmt.Errorf("capture retired memberships for mailbox %q: %w", mailbox, err)
		}
		if _, err := tx.Exec(`
			DELETE FROM imap_message_memberships
			WHERE source_id = ? AND mailbox = ?
		`, sourceID, mailbox); err != nil {
			return fmt.Errorf("retire IMAP memberships for mailbox %q: %w", mailbox, err)
		}
		if _, err := tx.Exec(`
			DELETE FROM imap_folder_state
			WHERE source_id = ? AND mailbox = ?
		`, sourceID, mailbox); err != nil {
			return fmt.Errorf("retire IMAP folder state for mailbox %q: %w", mailbox, err)
		}
	}
	return nil
}

func normalizeIMAPMailboxDeltas(deltas []IMAPMailboxDelta) ([]normalizedIMAPMailboxDelta, error) {
	normalized := make([]normalizedIMAPMailboxDelta, 0, len(deltas))
	for i, delta := range deltas {
		mailbox := delta.Mailbox
		var err error
		mailbox, err = mergeIMAPMailbox(mailbox, delta.State.Mailbox)
		if err != nil {
			return nil, fmt.Errorf("normalize IMAP delta %d: %w", i, err)
		}

		uidValidity := delta.State.UIDValidity
		for _, observation := range delta.Memberships {
			mailbox, err = mergeIMAPMailbox(mailbox, observation.Mailbox)
			if err != nil {
				return nil, fmt.Errorf("normalize IMAP delta %d: %w", i, err)
			}
			if observation.UIDValidity == 0 {
				continue
			}
			if uidValidity == 0 {
				uidValidity = observation.UIDValidity
				continue
			}
			if observation.UIDValidity != uidValidity {
				return nil, fmt.Errorf(
					"normalize IMAP delta %d: conflicting UIDVALIDITY values %d and %d",
					i, uidValidity, observation.UIDValidity,
				)
			}
		}
		normalized = append(normalized, normalizedIMAPMailboxDelta{
			delta: delta, mailbox: mailbox, uidValidity: uidValidity,
		})
	}
	return normalized, nil
}

func mergeIMAPMailbox(current, candidate string) (string, error) {
	if candidate == "" {
		return current, nil
	}
	if current == "" {
		return candidate, nil
	}
	if candidate != current {
		return "", fmt.Errorf("conflicting mailbox values %q and %q", current, candidate)
	}
	return current, nil
}

func captureIMAPMembershipMessageIDs(
	tx *loggedTx, affected map[int64]struct{}, query string, args ...any,
) error {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var messageID int64
		if err := rows.Scan(&messageID); err != nil {
			return err
		}
		affected[messageID] = struct{}{}
	}
	return rows.Err()
}

type imapMembershipResolver struct {
	tx          *loggedTx
	sourceID    int64
	rawMessages map[int64]map[[32]byte]int64
}

// primeRawIdentities snapshots durable raw candidates before mailbox resets,
// VANISHED removals, or topology retirement can change which canonical rows
// still have memberships. Later resolution is therefore independent of delta
// order within the transaction.
func (r *imapMembershipResolver) primeRawIdentities(
	deltas []normalizedIMAPMailboxDelta,
) error {
	for _, normalized := range deltas {
		for _, observation := range normalized.delta.Memberships {
			if observation.CanonicalSourceMessageID != "" ||
				observation.RawSHA256 == ([32]byte{}) {
				continue
			}
			if _, _, err := r.resolveRawSHA256(observation.RawSHA256, observation.RawSize); err != nil {
				return fmt.Errorf(
					"snapshot IMAP raw identity for mailbox %q UID %d: %w",
					normalized.mailbox, observation.UID, err,
				)
			}
		}
	}
	return nil
}

func (r *imapMembershipResolver) resolve(observation IMAPMembershipObservation) (int64, error) {
	var messageID int64
	if observation.CanonicalSourceMessageID != "" {
		err := r.tx.QueryRow(`
			SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
		`, r.sourceID, observation.CanonicalSourceMessageID).Scan(&messageID)
		if err == nil {
			return messageID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("resolve IMAP membership by canonical source message ID: %w", err)
		}
	}
	if observation.RawSHA256 != ([32]byte{}) {
		messageID, ok, err := r.resolveRawSHA256(observation.RawSHA256, observation.RawSize)
		if err != nil {
			return 0, err
		}
		if ok {
			return messageID, nil
		}
	}
	if observation.SourceMessageID != "" {
		err := r.tx.QueryRow(`
			SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?
		`, r.sourceID, observation.SourceMessageID).Scan(&messageID)
		if err == nil {
			return messageID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("resolve IMAP membership by source message ID: %w", err)
		}
	}
	if observation.RFC822MessageID != "" {
		for _, candidate := range imapRFC822MessageIDCandidates(observation.RFC822MessageID) {
			err := r.tx.QueryRow(`
				SELECT id FROM messages
				WHERE source_id = ? AND rfc822_message_id = ?
				ORDER BY id
				LIMIT 1
			`, r.sourceID, candidate).Scan(&messageID)
			if err == nil {
				return messageID, nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return 0, fmt.Errorf("resolve IMAP membership by RFC822 Message-ID: %w", err)
			}
		}
	}
	return 0, fmt.Errorf(
		"resolve IMAP membership for mailbox %q UID %d: no message matches source ID %q or RFC822 Message-ID %q",
		observation.Mailbox, observation.UID, observation.SourceMessageID, observation.RFC822MessageID)
}

func (r *imapMembershipResolver) resolveRawSHA256(
	digest [32]byte,
	rawSize int64,
) (int64, bool, error) {
	if r.rawMessages == nil {
		r.rawMessages = make(map[int64]map[[32]byte]int64)
	}
	messages, loaded := r.rawMessages[rawSize]
	if !loaded {
		messages = make(map[[32]byte]int64)
		r.rawMessages[rawSize] = messages
		query := `
			SELECT messages.id, message_raw.raw_data, message_raw.compression
			FROM messages
			JOIN message_raw ON message_raw.message_id = messages.id
			WHERE messages.source_id = ? AND message_raw.raw_format = 'mime'
			  AND messages.deleted_from_source_at IS NULL
			  AND EXISTS (
				SELECT 1 FROM imap_message_memberships membership
				WHERE membership.source_id = messages.source_id
				  AND membership.message_id = messages.id
			  )
		`
		args := []any{r.sourceID}
		if rawSize > 0 {
			query += ` AND messages.size_estimate = ?`
			args = append(args, rawSize)
		}
		query += ` ORDER BY messages.id`
		rows, err := r.tx.Query(query, args...)
		if err != nil {
			return 0, false, fmt.Errorf("load IMAP raw membership identities: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				messageID   int64
				rawData     []byte
				compression sql.NullString
			)
			if err := rows.Scan(&messageID, &rawData, &compression); err != nil {
				return 0, false, fmt.Errorf("scan IMAP raw membership identity: %w", err)
			}
			decoded, err := decodeMessageRaw(rawData, compression)
			if err != nil {
				return 0, false, fmt.Errorf("decode IMAP raw membership identity: %w", err)
			}
			rawDigest := sha256.Sum256(decoded)
			if existingMessageID, exists := messages[rawDigest]; !exists {
				messages[rawDigest] = messageID
			} else if existingMessageID != messageID {
				// Zero marks an ambiguous digest. The caller must fall back to
				// exact source identity instead of choosing an arbitrary row.
				messages[rawDigest] = 0
			}
		}
		if err := rows.Err(); err != nil {
			return 0, false, fmt.Errorf("iterate IMAP raw membership identities: %w", err)
		}
	}
	messageID, ok := messages[digest]
	return messageID, ok && messageID != 0, nil
}

func imapRFC822MessageIDCandidates(messageID string) []string {
	messageID = strings.TrimSpace(messageID)
	normalized := strings.TrimSuffix(strings.TrimPrefix(messageID, "<"), ">")
	candidates := []string{messageID}
	for _, candidate := range []string{normalized, "<" + normalized + ">"} {
		if candidate != "" && !slices.Contains(candidates, candidate) {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func imapMembershipMailboxes(
	tx *loggedTx, sourceID, messageID int64,
) ([]string, error) {
	rows, err := tx.Query(`
		SELECT mailbox FROM imap_message_memberships
		WHERE source_id = ? AND message_id = ?
		GROUP BY mailbox
		ORDER BY mailbox
	`, sourceID, messageID)
	if err != nil {
		return nil, fmt.Errorf("query memberships for IMAP message %d: %w", messageID, err)
	}
	defer func() { _ = rows.Close() }()
	var mailboxes []string
	for rows.Next() {
		var mailbox string
		if err := rows.Scan(&mailbox); err != nil {
			return nil, fmt.Errorf("scan membership for IMAP message %d: %w", messageID, err)
		}
		mailboxes = append(mailboxes, mailbox)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memberships for IMAP message %d: %w", messageID, err)
	}
	return mailboxes, nil
}

func ensureIMAPMailboxLabel(tx *loggedTx, sourceID int64, mailbox string) (int64, error) {
	var labelID int64
	err := tx.QueryRow(`
		SELECT id FROM labels WHERE source_id = ? AND source_label_id = ?
	`, sourceID, mailbox).Scan(&labelID)
	if err == nil {
		return labelID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("find label for IMAP mailbox %q: %w", mailbox, err)
	}
	labelID, err = ensureLabelWith(tx, sourceID, mailbox, mailbox, "user", nil)
	if err != nil {
		return 0, fmt.Errorf("ensure label for IMAP mailbox %q: %w", mailbox, err)
	}
	return labelID, nil
}

func sortedIMAPMessageIDs(ids map[int64]struct{}) []int64 {
	sorted := make([]int64, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	slices.Sort(sorted)
	return sorted
}
