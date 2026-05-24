package store

import (
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

	if err := s.dedupeParticipantsByPhone(); err != nil {
		return fmt.Errorf("dedupe participants by phone: %w", err)
	}

	if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_participants_phone`); err != nil {
		return fmt.Errorf("drop idx_participants_phone: %w", err)
	}

	if _, err := s.db.Exec(`
		CREATE UNIQUE INDEX idx_participants_phone ON participants(phone_number)
		    WHERE phone_number IS NOT NULL
	`); err != nil {
		return fmt.Errorf("create unique idx_participants_phone: %w", err)
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
func (s *Store) dedupeParticipantsByPhone() error {
	return s.withTx(func(tx *loggedTx) error {
		// Pull every participant id involved in a duplicate-phone
		// group, ordered so the per-phone winner (lowest id) comes
		// first within each group.
		rows, err := tx.Query(`
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
				if err := mergeParticipant(tx, winner, loser); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// mergeParticipant moves every reference from loser onto winner,
// deleting any rows whose new (winner-scoped) key would clash with an
// existing row, then deletes loser from participants.
func mergeParticipant(tx *loggedTx, winner, loser int64) error {
	// (1) message_recipients UNIQUE(message_id, participant_id, recipient_type)
	if _, err := tx.Exec(`
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
	if _, err := tx.Exec(
		`UPDATE message_recipients SET participant_id = ? WHERE participant_id = ?`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("repoint message_recipients (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (2) conversation_participants PRIMARY KEY (conversation_id, participant_id)
	if _, err := tx.Exec(`
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
	if _, err := tx.Exec(
		`UPDATE conversation_participants SET participant_id = ? WHERE participant_id = ?`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("repoint conversation_participants (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (3) reactions UNIQUE(message_id, participant_id, reaction_type, reaction_value)
	if _, err := tx.Exec(`
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
	if _, err := tx.Exec(
		`UPDATE reactions SET participant_id = ? WHERE participant_id = ?`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("repoint reactions (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (4) messages.sender_id — nullable, no UNIQUE, plain UPDATE.
	if _, err := tx.Exec(
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
	if _, err := tx.Exec(`
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
	if _, err := tx.Exec(
		`UPDATE participant_identifiers SET participant_id = ? WHERE participant_id = ?`,
		winner, loser,
	); err != nil {
		return fmt.Errorf("repoint participant_identifiers (loser=%d, winner=%d): %w", loser, winner, err)
	}

	// (6) Finally drop the loser. participant_identifiers cascades; the
	// other FKs are already cleared by the repoints above.
	if _, err := tx.Exec(`DELETE FROM participants WHERE id = ?`, loser); err != nil {
		return fmt.Errorf("delete loser participant id=%d: %w", loser, err)
	}
	return nil
}
