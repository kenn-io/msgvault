package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/calevent"
)

// RoomAddressSQL uses typed resource evidence only while its parent remains
// locally live. Provider cancellation or deletion does not erase that evidence.
func RoomAddressSQL(email string) string {
	return calevent.RoomAddressSQL(email, `EXISTS (SELECT 1 FROM calendar_event_attendees room_attendee WHERE room_attendee.role = 'room' AND lower(trim(room_attendee.email)) = lower(trim(`+email+`)) AND EXISTS (SELECT 1 FROM messages room_message WHERE room_message.id = room_attendee.message_id AND room_message.message_type = 'calendar_event' AND room_message.deleted_at IS NULL))`)
}

func personRoomContactsSQL(personID string) string {
	return `SELECT address_kind AS kind, normalized_value AS value FROM person_contact_points WHERE person_id = ` + personID + ` AND active_until IS NULL AND superseded_at IS NULL AND trim(normalized_value) <> ''
 UNION SELECT 'email', room_bound_participant.email_address FROM person_participants room_bound_link JOIN participants room_bound_participant ON room_bound_participant.id = room_bound_link.participant_id WHERE room_bound_link.person_id = ` + personID + ` AND trim(coalesce(room_bound_participant.email_address,'')) <> ''
 UNION SELECT 'phone', room_bound_participant.phone_number FROM person_participants room_bound_link JOIN participants room_bound_participant ON room_bound_participant.id = room_bound_link.participant_id WHERE room_bound_link.person_id = ` + personID + ` AND trim(coalesce(room_bound_participant.phone_number,'')) <> ''
 UNION SELECT room_bound_identifier.identifier_type, room_bound_identifier.identifier_value FROM person_participants room_bound_link JOIN participant_identifiers room_bound_identifier ON room_bound_identifier.participant_id = room_bound_link.participant_id WHERE room_bound_link.person_id = ` + personID + ` AND trim(room_bound_identifier.identifier_value) <> ''`
}

func visiblePersonSQL(personID string) string {
	contacts := personRoomContactsSQL(personID)
	return `(NOT EXISTS (SELECT 1 FROM (` + contacts + `) person_room_contacts) OR EXISTS (SELECT 1 FROM (` + contacts + `) person_room_contacts WHERE kind <> 'email' OR NOT ` + RoomAddressSQL("person_room_contacts.value") + `))`
}

func (s *Store) requireVisiblePersonTx(ctx context.Context, tx *loggedTx, personID int64) error {
	var visible bool
	// Bind once, so the portable predicate can reuse the ID without repeated args.
	err := tx.QueryRowContext(ctx, `SELECT `+visiblePersonSQL("room_person.id")+` FROM persons room_person WHERE room_person.id = ?`, personID).Scan(&visible)
	if err == sql.ErrNoRows {
		return ErrPersonNotFound
	}
	if err != nil {
		return fmt.Errorf("read visible person: %w", err)
	}
	if !visible {
		return ErrPersonNotFound
	}
	return nil
}

func (s *Store) visibleContactPointsTx(ctx context.Context, tx *loggedTx, points []PersonContactPoint) ([]PersonContactPoint, error) {
	result := make([]PersonContactPoint, 0, len(points))
	for _, point := range points {
		if point.AddressKind == ContactAddressEmail {
			var room bool
			if err := tx.QueryRowContext(ctx, `SELECT `+RoomAddressSQL("room_value.email")+` FROM (SELECT ? AS email) room_value`, point.NormalizedValue).Scan(&room); err != nil {
				return nil, fmt.Errorf("classify person contact: %w", err)
			}
			if room {
				continue
			}
		}
		result = append(result, point)
	}
	return result, nil
}

// roomAttendeeDirectoryDirtySQL limits attendee-row invalidation to people
// with email evidence whose room classification can change. Other contact
// kinds are always human, and cache fact invalidation stays conservative.
func roomAttendeeDirectoryDirtySQL(refs []string) string {
	addresses := make([]string, 0, len(refs))
	for _, ref := range refs {
		addresses = append(addresses, `SELECT lower(trim(`+ref+`.email)) WHERE `+ref+`.role = 'room' AND trim(`+ref+`.email) <> ''`)
	}
	emails := strings.Join(addresses, " UNION ")
	return `INSERT INTO directory_projection_dirty(person_id)
 SELECT person_id FROM person_contact_points
 WHERE address_kind = 'email' AND active_until IS NULL AND superseded_at IS NULL
 AND lower(trim(normalized_value)) IN (` + emails + `)
 UNION SELECT person_id FROM person_participants WHERE participant_id IN (
  SELECT id FROM participants WHERE lower(trim(email_address)) IN (` + emails + `)
  UNION SELECT participant_id FROM participant_identifiers
  WHERE identifier_type = 'email' AND lower(trim(identifier_value)) IN (` + emails + `)
 ) ON CONFLICT(person_id) DO NOTHING;`
}

func (s *Store) installRoomDirectoryInvalidation(ctx context.Context) error {
	const allPeople = `INSERT INTO directory_projection_dirty(person_id) SELECT id FROM persons WHERE true ON CONFLICT(person_id) DO NOTHING;`
	if s.IsPostgreSQL() {
		for _, statement := range []string{
			`CREATE OR REPLACE FUNCTION directory_rooms_attendee_dirty() RETURNS trigger AS $$
    BEGIN
     IF TG_OP = 'INSERT' THEN ` + roomAttendeeDirectoryDirtySQL([]string{"NEW"}) + `
     ELSIF TG_OP = 'DELETE' THEN ` + roomAttendeeDirectoryDirtySQL([]string{"OLD"}) + `
     ELSE ` + roomAttendeeDirectoryDirtySQL([]string{"OLD", "NEW"}) + `
     END IF;
     RETURN NULL;
    END $$ LANGUAGE plpgsql`,
			`CREATE OR REPLACE FUNCTION directory_rooms_mark_bound_dirty() RETURNS trigger AS $$
    DECLARE old_id bigint; new_id bigint;
    BEGIN
     IF TG_OP <> 'INSERT' THEN old_id := (to_jsonb(OLD)->>TG_ARGV[0])::bigint; END IF;
     IF TG_OP <> 'DELETE' THEN new_id := (to_jsonb(NEW)->>TG_ARGV[0])::bigint; END IF;
     INSERT INTO directory_projection_dirty(person_id) SELECT person_id FROM person_participants WHERE participant_id IN (old_id,new_id) ON CONFLICT DO NOTHING;
     RETURN NULL;
    END $$ LANGUAGE plpgsql`,
			`CREATE OR REPLACE FUNCTION directory_rooms_message_dirty() RETURNS trigger AS $$
    BEGIN
     IF TG_OP = 'DELETE' THEN
      IF OLD.message_type = 'calendar_event' THEN ` + allPeople + ` END IF;
     ELSIF (OLD.message_type = 'calendar_event' OR NEW.message_type = 'calendar_event')
       AND (OLD.deleted_at IS DISTINCT FROM NEW.deleted_at OR OLD.message_type IS DISTINCT FROM NEW.message_type) THEN ` + allPeople + `
     END IF;
     RETURN NULL;
    END $$ LANGUAGE plpgsql`,
			`CREATE OR REPLACE TRIGGER directory_rooms_calendar_event_attendees AFTER INSERT OR UPDATE OR DELETE ON calendar_event_attendees FOR EACH ROW EXECUTE FUNCTION directory_rooms_attendee_dirty()`,
			`CREATE OR REPLACE TRIGGER directory_rooms_messages AFTER UPDATE OF deleted_at,message_type OR DELETE ON messages FOR EACH ROW EXECUTE FUNCTION directory_rooms_message_dirty()`,
			`CREATE OR REPLACE TRIGGER directory_rooms_participants AFTER INSERT OR UPDATE OR DELETE ON participants FOR EACH ROW EXECUTE FUNCTION directory_rooms_mark_bound_dirty('id')`,
			`CREATE OR REPLACE TRIGGER directory_rooms_participant_identifiers AFTER INSERT OR UPDATE OR DELETE ON participant_identifiers FOR EACH ROW EXECUTE FUNCTION directory_rooms_mark_bound_dirty('participant_id')`,
			`CREATE OR REPLACE TRIGGER directory_rooms_person_participants AFTER INSERT OR UPDATE OR DELETE ON person_participants FOR EACH ROW EXECUTE FUNCTION directory_projection_mark_dirty('person_id')`,
		} {
			if _, err := s.db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("install room directory trigger: %w", err)
			}
		}
		return nil
	}
	for _, item := range []struct{ table, column string }{{"calendar_event_attendees", ""}, {"participants", "id"}, {"participant_identifiers", "participant_id"}, {"person_participants", personMergePersonIDColumn}} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			body := allPeople
			when := ""
			if item.table == "calendar_event_attendees" {
				var refs, roles []string
				if event != "INSERT" {
					refs = append(refs, "OLD")
					roles = append(roles, "OLD.role = 'room'")
				}
				if event != "DELETE" {
					refs = append(refs, "NEW")
					roles = append(roles, "NEW.role = 'room'")
				}
				body = roomAttendeeDirectoryDirtySQL(refs)
				when = " WHEN " + strings.Join(roles, " OR ")
			} else if item.column != "" {
				var refs []string
				if event != "INSERT" {
					refs = append(refs, "OLD."+item.column)
				}
				if event != "DELETE" {
					refs = append(refs, "NEW."+item.column)
				}
				if item.table == "person_participants" {
					body = `INSERT INTO directory_projection_dirty(person_id) SELECT id FROM persons WHERE id IN (` + strings.Join(refs, ",") + `) ON CONFLICT(person_id) DO NOTHING;`
				} else {
					body = `INSERT INTO directory_projection_dirty(person_id) SELECT person_id FROM person_participants WHERE participant_id IN (` + strings.Join(refs, ",") + `) ON CONFLICT(person_id) DO NOTHING;`
				}
			}
			if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS directory_rooms_`+item.table+`_`+event+` AFTER `+event+` ON `+item.table+when+` BEGIN `+body+` END`); err != nil {
				return err
			}
		}
	}
	for _, statement := range []string{
		`CREATE TRIGGER IF NOT EXISTS directory_rooms_messages_update AFTER UPDATE OF deleted_at,message_type ON messages WHEN (OLD.message_type='calendar_event' OR NEW.message_type='calendar_event') AND (OLD.deleted_at IS NOT NEW.deleted_at OR OLD.message_type IS NOT NEW.message_type) BEGIN ` + allPeople + ` END`,
		`CREATE TRIGGER IF NOT EXISTS directory_rooms_messages_delete AFTER DELETE ON messages WHEN OLD.message_type='calendar_event' BEGIN ` + allPeople + ` END`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func participantObservedContactsSQL(participantID string) string {
	return `SELECT 'email' AS kind,email_address AS value FROM participants WHERE id=` + participantID + ` AND trim(coalesce(email_address,'')) <> ''
 UNION ALL SELECT 'phone',phone_number FROM participants WHERE id=` + participantID + ` AND trim(coalesce(phone_number,'')) <> ''
 UNION ALL SELECT identifier_type,identifier_value FROM participant_identifiers WHERE participant_id=` + participantID + ` AND trim(identifier_value) <> ''`
}

func participantObservedRoomOnlySQL(participantID string) string {
	contacts := participantObservedContactsSQL(participantID)
	return `(EXISTS(SELECT 1 FROM (` + contacts + `) observed_contacts) AND NOT EXISTS(SELECT 1 FROM (` + contacts + `) observed_contacts WHERE kind <> 'email' OR NOT ` + RoomAddressSQL("observed_contacts.value") + `))`
}

func participantRoomOnlySQL(participantID string) string {
	contacts := participantObservedContactsSQL(participantID) + `
 UNION ALL SELECT pc.address_kind, pc.normalized_value FROM person_contact_points pc
 WHERE pc.active_until IS NULL AND pc.superseded_at IS NULL AND trim(pc.normalized_value) <> ''
 AND EXISTS (SELECT 1 FROM person_participants pp WHERE pp.person_id=pc.person_id AND pp.participant_id=` + participantID + `)`
	return `(EXISTS(SELECT 1 FROM (` + contacts + `) participant_room_contacts) AND NOT EXISTS(SELECT 1 FROM (` + contacts + `) participant_room_contacts WHERE kind <> 'email' OR NOT ` + RoomAddressSQL("participant_room_contacts.value") + `))`
}

// GetVisibleParticipantIdentityContext keeps graph IDs and link provenance while
// hiding room contacts and names from people response hydration. The raw reader
// remains available to authoritative identity/export paths.
func (s *Store) GetVisibleParticipantIdentityContext(ctx context.Context, ids []int64) (*ParticipantIdentityContext, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	details, err := s.GetParticipantIdentityContext(ctx, ids)
	if err != nil {
		return nil, err
	}
	reader := s.snapshotReader(ctx)
	rooms := make(map[string]bool)
	classify := func(email string) (bool, error) {
		normalized := strings.ToLower(strings.TrimSpace(email))
		if room, ok := rooms[normalized]; ok {
			return room, nil
		}
		var room bool
		err := reader.QueryRowContext(ctx, `SELECT `+RoomAddressSQL("room_input.email")+` FROM (SELECT ? AS email) room_input`, email).Scan(&room)
		if err != nil {
			return false, err
		}
		rooms[normalized] = room
		return room, nil
	}
	for i := range details.Members {
		member := &details.Members[i]
		room, err := classify(member.Email)
		if err != nil {
			return nil, err
		}
		if room {
			member.Email = ""
		}
		var roomOnly bool
		err = reader.QueryRowContext(ctx, `SELECT (`+participantObservedRoomOnlySQL("room_p.id")+` OR `+participantRoomOnlySQL("room_p.id")+`) FROM participants room_p WHERE id=?`, member.ParticipantID).Scan(&roomOnly)
		if err != nil {
			return nil, err
		}
		if roomOnly {
			member.DisplayName = ""
		}
	}
	visible := make([]ParticipantIdentifierContext, 0, len(details.Identifiers))
	for _, identifier := range details.Identifiers {
		if identifier.Type == "email" {
			room, err := classify(identifier.Value)
			if err != nil {
				return nil, err
			}
			if room {
				continue
			}
		}
		visible = append(visible, identifier)
	}
	details.Identifiers = visible
	for email, room := range rooms {
		if room {
			details.RoomEmails = append(details.RoomEmails, email)
		}
	}
	return details, nil
}
