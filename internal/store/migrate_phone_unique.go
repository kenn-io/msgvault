package store

import (
	"context"
	"database/sql"
	"fmt"
)

const migrationPhoneUniqueIndex = "participants_phone_unique_index"

// ensureParticipantsPhoneUniqueIndex upgrades legacy databases whose
// idx_participants_phone was created as a non-unique partial index
// before the schema flipped it to UNIQUE. Because schema.sql now uses
// `CREATE UNIQUE INDEX IF NOT EXISTS idx_participants_phone …` and
// `IF NOT EXISTS` is satisfied by any index of the same name, legacy
// DBs would silently keep the non-unique index and EnsureParticipant-
// ByPhone's `ON CONFLICT (phone_number)` would have no matching unique
// constraint to bind to. The fix is a one-shot migration tracked in
// applied_migrations:
//
//  1. dedupe rows that share a phone_number (re-point participant_id
//     FKs from losers to the winner, then delete the losers),
//  2. drop the index unconditionally (drop is harmless if it was
//     already unique),
//  3. recreate it as UNIQUE.
//
// Works identically on SQLite and PostgreSQL (DROP INDEX IF EXISTS
// and partial UNIQUE indexes are supported on both).
func (s *Store) ensureParticipantsPhoneUniqueIndex() error {
	applied, err := s.IsMigrationApplied(migrationPhoneUniqueIndex)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}

	// Route the whole migration (dedupe + DROP + CREATE UNIQUE INDEX) through
	// ONE runMaintenance transaction, mirroring the attachment unique-index
	// migration in InitSchema. runMaintenance disables the pool-wide 30s
	// statement_timeout for this tx: both the dedupe DELETEs/UPDATEs and the
	// UNIQUE-index build over the full participants table scale with participant
	// count and can exceed 30s on a large archive (finding S1). Running them in
	// one tx also guarantees the index is built against the just-deduped table.
	// No-op timeout reset on SQLite.
	if err := s.runMaintenance(context.Background(), func(ctx context.Context, tx *loggedTx) error {
		if err := s.dedupeParticipantsByPhone(ctx, tx); err != nil {
			return fmt.Errorf("dedupe participants by phone: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_participants_phone`); err != nil {
			return fmt.Errorf("drop idx_participants_phone: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			CREATE UNIQUE INDEX idx_participants_phone ON participants(phone_number)
			    WHERE phone_number IS NOT NULL
		`); err != nil {
			return fmt.Errorf("create unique idx_participants_phone: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	return s.MarkMigrationApplied(migrationPhoneUniqueIndex)
}

// dedupeParticipantsByPhone merges rows that share a non-null
// phone_number. For each duplicate group the lowest id is kept;
// foreign-key references on the losers (message_recipients.
// participant_id, conversation_participants.participant_id,
// reactions.participant_id, messages.sender_id) are repointed to the
// winner. Rows that would violate a unique constraint after the
// repoint are deleted first so the merge is conflict-free. Then the
// loser participants are deleted, which CASCADEs any remaining
// references (participant_identifiers etc.).
//
// Runs on the caller-supplied maintenance transaction (ctx, tx) so the dedupe
// and the subsequent UNIQUE-index build share one statement_timeout-disabled tx
// (finding S1) and the index is built against the just-deduped table.
func (s *Store) dedupeParticipantsByPhone(ctx context.Context, tx *loggedTx) error {
	// Pull every participant id involved in a duplicate-phone
	// group, ordered so the per-phone winner (lowest id) comes
	// first within each group.
	rows, err := tx.QueryContext(ctx, `
		SELECT phone_number, id FROM participants
		 WHERE phone_number IS NOT NULL
		   AND phone_number IN (
		       SELECT phone_number FROM participants
		        WHERE phone_number IS NOT NULL
		        GROUP BY phone_number
		       HAVING COUNT(*) > 1
		   )
		 ORDER BY phone_number, id
	`)
	if err != nil {
		return fmt.Errorf("scan duplicate phone groups: %w", err)
	}
	groups := map[string][]int64{}
	for rows.Next() {
		var phone string
		var id int64
		if err := rows.Scan(&phone, &id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan phone dup row: %w", err)
		}
		groups[phone] = append(groups[phone], id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate phone dup rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close phone dup rows: %w", err)
	}

	for _, ids := range groups {
		if len(ids) < 2 {
			continue
		}
		winner := ids[0]
		losers := ids[1:]
		for _, loser := range losers {
			if err := s.mergeParticipant(ctx, tx, winner, loser); err != nil {
				return err
			}
		}
	}
	return nil
}

// mergeParticipant moves every reference from loser onto winner,
// deleting any rows whose new (winner-scoped) key would clash with an
// existing row, fills the winner's empty metadata fields (email_address,
// domain, display_name) from the loser, then deletes loser from
// participants.
func (s *Store) mergeParticipant(ctx context.Context, tx *loggedTx, winner, loser int64) error {
	// (1) message_recipients UNIQUE(message_id, participant_id, recipient_type)
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM message_recipients
		 WHERE participant_id = ?
		   AND EXISTS (
		       SELECT 1 FROM message_recipients mr2
		        WHERE mr2.participant_id = ?
		          AND mr2.message_id = message_recipients.message_id
		          AND mr2.recipient_type = message_recipients.recipient_type
		   )
	`, loser, winner); err != nil {
		return fmt.Errorf("dedupe message_recipients (loser=%d, winner=%d): %w", loser, winner, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE message_recipients SET participant_id = ? WHERE participant_id = ?`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("repoint message_recipients (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (2) conversation_participants PRIMARY KEY (conversation_id, participant_id)
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM conversation_participants
		 WHERE participant_id = ?
		   AND EXISTS (
		       SELECT 1 FROM conversation_participants cp2
		        WHERE cp2.participant_id = ?
		          AND cp2.conversation_id = conversation_participants.conversation_id
		   )
	`, loser, winner); err != nil {
		return fmt.Errorf("dedupe conversation_participants (loser=%d, winner=%d): %w", loser, winner, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversation_participants SET participant_id = ? WHERE participant_id = ?`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("repoint conversation_participants (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (3) reactions UNIQUE(message_id, participant_id, reaction_type, reaction_value)
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM reactions
		 WHERE participant_id = ?
		   AND EXISTS (
		       SELECT 1 FROM reactions r2
		        WHERE r2.participant_id = ?
		          AND r2.message_id = reactions.message_id
		          AND r2.reaction_type = reactions.reaction_type
		          AND r2.reaction_value = reactions.reaction_value
		   )
	`, loser, winner); err != nil {
		return fmt.Errorf("dedupe reactions (loser=%d, winner=%d): %w", loser, winner, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE reactions SET participant_id = ? WHERE participant_id = ?`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("repoint reactions (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (4) messages.sender_id — nullable, no UNIQUE, plain UPDATE.
	if _, err := tx.ExecContext(ctx,
		`UPDATE messages SET sender_id = ? WHERE sender_id = ?`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("repoint messages.sender_id (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (5) participant_identifiers has ON DELETE CASCADE and no
	// participant_id-only UNIQUE; the (identifier_type, identifier_value)
	// UNIQUE is global, not per-participant, so duplicate identifier rows
	// across the two participants must already have failed at insert
	// time. Move them onto the winner and rely on the unique constraint
	// failing only when a true duplicate exists; in practice phone-keyed
	// participants rarely have non-phone identifiers attached.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM participant_identifiers
		 WHERE participant_id = ?
		   AND EXISTS (
		       SELECT 1 FROM participant_identifiers pi2
		        WHERE pi2.participant_id = ?
		          AND pi2.identifier_type = participant_identifiers.identifier_type
		          AND pi2.identifier_value = participant_identifiers.identifier_value
		   )
	`, loser, winner); err != nil {
		return fmt.Errorf("dedupe participant_identifiers (loser=%d, winner=%d): %w", loser, winner, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE participant_identifiers SET participant_id = ? WHERE participant_id = ?`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("repoint participant_identifiers (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (6) Repoint (and, if needed, restructure) any link edges referencing
	// loser before the delete below drops them via ON DELETE CASCADE.
	if err := s.rewriteLinksForMerge(tx, loser, winner); err != nil {
		return fmt.Errorf("rewrite participant links (loser=%d, winner=%d): %w", loser, winner, err)
	}
	// Bump unconditionally, even when the merge touched no link edges: see
	// the matching comment in MergeParticipants (messages.go).
	if _, err := s.bumpIdentityRevision(tx); err != nil {
		return fmt.Errorf("bump identity revision (loser=%d, winner=%d): %w", loser, winner, err)
	}
	// Also bump the account-identity revision: the merge repoints
	// messages.sender_id, so a merge involving the sender of any message
	// with a baked is_from_me leaves that flag stale in the message
	// Parquet shards, which only a full rebuild re-derives.
	if err := s.bumpAccountIdentityRevision(tx); err != nil {
		return fmt.Errorf("bump account identity revision (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (7) Preserve contact metadata: fill the winner's empty fields from
	// the loser before the delete below discards them, mirroring the
	// coalesce in MergeParticipants (messages.go). email_address is
	// UNIQUE (idx_participants_email on both backends), so the loser must
	// release its value before the winner can take it — inside this same
	// tx, so a failure rolls the whole migration back. phone_number needs
	// no transfer: winner and loser share it by construction (that is
	// what made them a duplicate group). display_name is coalesced too
	// (phone-keyed participants often carry a name and nothing else).
	// With multiple losers this runs once per loser in ascending-id
	// order, so for each field the lowest-id loser holding a value wins;
	// later losers only fill fields still empty on the winner.
	var loserEmail, loserDomain, loserName sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT NULLIF(email_address, ''), NULLIF(domain, ''), NULLIF(display_name, '')
		  FROM participants WHERE id = ?`, loser,
	).Scan(&loserEmail, &loserDomain, &loserName); err != nil {
		return fmt.Errorf("read loser metadata (loser=%d): %w", loser, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE participants SET email_address = NULL WHERE id = ?`, loser,
	); err != nil {
		return fmt.Errorf("release loser email (loser=%d): %w", loser, err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE participants SET
			email_address = COALESCE(NULLIF(email_address, ''), ?),
			domain        = COALESCE(NULLIF(domain, ''), ?),
			display_name  = COALESCE(NULLIF(display_name, ''), ?)
		WHERE id = ?`, loserEmail, loserDomain, loserName, winner,
	); err != nil {
		return fmt.Errorf("coalesce metadata onto winner (winner=%d, loser=%d): %w", winner, loser, err)
	}

	// (8) Finally drop the loser. participant_identifiers cascades; the
	// other FKs are already cleared by the repoints above.
	if _, err := tx.ExecContext(ctx, `DELETE FROM participants WHERE id = ?`, loser); err != nil {
		return fmt.Errorf("delete loser participant id=%d: %w", loser, err)
	}
	return nil
}
