package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Native writers enter the same database gate as delivery admission. Statement
// triggers on PostgreSQL acquire it before tuple locks; admission deliberately
// takes no identity/native row locks. Binding epochs survive away/back changes.
type deliveryBindingTable struct {
	table, kind, id string
	columns         []string
}

func (s *Store) ensureDeliveryPolicyInfrastructure(ctx context.Context) error {
	tables := []deliveryBindingTable{
		{"persons", string(AttributeObjectPerson), "id", []string{"vcard_uid", "revision"}},
		{"person_participants", string(AttributeObjectPerson), personMergePersonIDColumn, nil},
		{"person_contact_points", "contact_point", "id", []string{personMergePersonIDColumn, "address_kind", "service_id", "scope_kind", "scope_value", "normalized_value", "active_from", "active_until", "superseded_at"}},
		{"participants", "participant", "id", []string{"email_address", "phone_number", "canonical_id"}},
		{"participant_identifiers", "participant", "participant_id", []string{"participant_id", "identifier_type", "identifier_value", "is_primary", "service_id", "scope_kind", "scope_value"}},
		{"participant_contact_observations", "participant", "participant_id", []string{"participant_id", "source_id", "address_kind", "service_id", "scope_kind", "scope_value", "provider_user_id", "normalized_value", "active_from", "active_until", "superseded_at"}},
		{"sources", "source", "id", []string{"source_type", "identifier", "sync_config", "oauth_app", "google_user_id"}},
		{"conversations", "conversation", "id", []string{"source_id", "source_conversation_id", "conversation_type", "metadata"}},
		{"conversation_participants", "conversation", "conversation_id", nil},
	}
	// PR1114 owns this native lifecycle table. Installing its fence after the
	// schema is present avoids creating a competing source-lifecycle model.
	present, err := s.tableExistsContext(ctx, "source_settings")
	if err != nil {
		return err
	}
	if present {
		tables = append(tables, deliveryBindingTable{"source_settings", "source", "source_id", nil})
	}
	if s.IsPostgreSQL() {
		if _, err = s.db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION delivery_route_metadata(value JSONB) RETURNS JSONB LANGUAGE plpgsql IMMUTABLE AS $$
 DECLARE route JSONB; BEGIN route:=value->'messaging_route'; IF jsonb_typeof(route)='object' THEN RETURN route - 'observed_at' - 'network_label'; END IF; RETURN jsonb_build_object('invalid',value);
 EXCEPTION WHEN invalid_text_representation THEN RETURN jsonb_build_object('invalid',value); END $$;
 CREATE OR REPLACE FUNCTION delivery_enter_admission() RETURNS trigger LANGUAGE plpgsql AS $$
   BEGIN PERFORM pg_advisory_xact_lock_shared(hashtextextended('msgvault.delivery_admission:' || current_schema(), 0)); RETURN NULL; END $$`); err != nil {
			return fmt.Errorf("create delivery gate function: %w", err)
		}
	}
	for _, table := range tables {
		if err := s.installDeliveryBindingTriggers(ctx, table); err != nil {
			return fmt.Errorf("fence %s: %w", table.table, err)
		}
	}
	for _, table := range []string{"messages", "message_recipients", "person_uid_aliases"} {
		if err := s.installDeliveryEvidenceFence(ctx, table); err != nil {
			return err
		}
	}
	if err := s.installDeliveryEmailEvidenceTracking(ctx); err != nil {
		return fmt.Errorf("track participant/source email evidence: %w", err)
	}
	return s.installDeliveryMarkerTriggers(ctx)
}

func (s *Store) deliveryBindingChanged(t deliveryBindingTable) string {
	if len(t.columns) == 0 {
		return "TRUE"
	}
	comparisons := make([]string, 0, len(t.columns))
	for _, column := range t.columns {
		old, newValue := "OLD."+column, "NEW."+column
		if t.table == "conversations" && column == "metadata" {
			if s.IsPostgreSQL() {
				old = "delivery_route_metadata(OLD.metadata)"
				newValue = "delivery_route_metadata(NEW.metadata)"
			} else {
				old = "CASE WHEN json_valid(OLD.metadata) THEN CASE WHEN json_type(OLD.metadata,'$.messaging_route')='object' THEN json_remove(json_extract(OLD.metadata,'$.messaging_route'),'$.observed_at','$.network_label') ELSE OLD.metadata END ELSE OLD.metadata END"
				newValue = "CASE WHEN json_valid(NEW.metadata) THEN CASE WHEN json_type(NEW.metadata,'$.messaging_route')='object' THEN json_remove(json_extract(NEW.metadata,'$.messaging_route'),'$.observed_at','$.network_label') ELSE NEW.metadata END ELSE NEW.metadata END"
			}
		}
		comparison := old + " IS NOT " + newValue
		if s.IsPostgreSQL() {
			comparison = old + " IS DISTINCT FROM " + newValue
		}
		comparisons = append(comparisons, "("+comparison+")")
	}
	return strings.Join(comparisons, " OR ")
}
func deliveryEpochSQL(kind, id string) string {
	return `UPDATE delivery_admission_lock SET generation=generation+1 WHERE singleton=1;
 INSERT INTO delivery_binding_versions(kind,target_id,version,generation) VALUES ('` + kind + `',` + id + `,1,(SELECT generation FROM delivery_admission_lock WHERE singleton=1))
 ON CONFLICT(kind,target_id) DO UPDATE SET version=delivery_binding_versions.version+1,generation=excluded.generation;`
}

func (s *Store) installDeliveryBindingTriggers(ctx context.Context, t deliveryBindingTable) error {
	changed := s.deliveryBindingChanged(t)
	if s.IsPostgreSQL() {
		statement := `CREATE OR REPLACE FUNCTION delivery_epoch_` + t.table + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
   IF TG_OP='INSERT' THEN ` + deliveryEpochSQL(t.kind, "NEW."+t.id) + `
   ELSIF TG_OP='DELETE' THEN ` + deliveryEpochSQL(t.kind, "OLD."+t.id) + `
   ELSIF ` + changed + ` THEN ` + deliveryEpochSQL(t.kind, "NEW."+t.id) + `
    IF OLD.` + t.id + ` IS DISTINCT FROM NEW.` + t.id + ` THEN ` + deliveryEpochSQL(t.kind, "OLD."+t.id) + ` END IF;
   END IF; RETURN NULL; END $$;
   DROP TRIGGER IF EXISTS delivery_gate ON ` + t.table + `;
   CREATE TRIGGER delivery_gate BEFORE INSERT OR UPDATE OR DELETE ON ` + t.table + ` FOR EACH STATEMENT EXECUTE FUNCTION delivery_enter_admission();
   DROP TRIGGER IF EXISTS delivery_epoch ON ` + t.table + `;
   CREATE TRIGGER delivery_epoch AFTER INSERT OR UPDATE OR DELETE ON ` + t.table + ` FOR EACH ROW EXECUTE FUNCTION delivery_epoch_` + t.table + `();`
		_, err := s.db.ExecContext(ctx, statement)
		return err
	}
	for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
		name := "delivery_" + t.table + "_" + strings.ToLower(operation)
		when := ""
		body := deliveryEpochSQL(t.kind, "NEW."+t.id)
		if operation == "DELETE" {
			body = deliveryEpochSQL(t.kind, "OLD."+t.id)
		}
		if operation == "UPDATE" {
			when = " WHEN " + changed
			body += ` INSERT INTO delivery_binding_versions(kind,target_id,version,generation) SELECT '` + t.kind + `',OLD.` + t.id + `,1,(SELECT generation FROM delivery_admission_lock WHERE singleton=1) WHERE OLD.` + t.id + ` IS NOT NEW.` + t.id + ` ON CONFLICT(kind,target_id) DO UPDATE SET version=delivery_binding_versions.version+1,generation=excluded.generation;`
		}
		_, err := s.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+name+`;
   DROP TRIGGER IF EXISTS `+name+`_epoch;
   CREATE TRIGGER `+name+` BEFORE `+operation+` ON `+t.table+` BEGIN UPDATE delivery_admission_lock SET singleton=singleton WHERE singleton=1; END;
   CREATE TRIGGER `+name+`_epoch AFTER `+operation+` ON `+t.table+when+` BEGIN `+body+` END;`)
		if err != nil {
			return err
		}
	}
	return nil
}

// The re-anchor marker is native archive_metadata state, not a policy flag.
// Its insertion AND removal change the source epoch, so explicit acceptance
// cannot revive a grant recorded before loss of account continuity.
func (s *Store) installDeliveryMarkerTriggers(ctx context.Context) error {
	prefix := beeperReanchorRequiredMarkerPrefix
	if s.IsPostgreSQL() {
		_, err := s.db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION delivery_epoch_marker() RETURNS trigger LANGUAGE plpgsql AS $$
   DECLARE marker TEXT; BEGIN
   IF TG_OP <> 'INSERT' THEN marker:=OLD.key; IF marker ~ '^beeper[.]reanchor_required:[0-9]{1,18}$' THEN `+deliveryEpochSQL("source", `CAST(substring(marker FROM `+strconv.Itoa(len(prefix)+1)+`) AS BIGINT)`)+` END IF; END IF;
   IF TG_OP <> 'DELETE' THEN marker:=NEW.key; IF marker ~ '^beeper[.]reanchor_required:[0-9]{1,18}$' THEN `+deliveryEpochSQL("source", `CAST(substring(marker FROM `+strconv.Itoa(len(prefix)+1)+`) AS BIGINT)`)+` END IF; END IF;
   RETURN NULL; END $$;
   DROP TRIGGER IF EXISTS delivery_gate ON archive_metadata;
   CREATE TRIGGER delivery_gate BEFORE INSERT OR UPDATE OR DELETE ON archive_metadata FOR EACH STATEMENT EXECUTE FUNCTION delivery_enter_admission();
   DROP TRIGGER IF EXISTS delivery_epoch ON archive_metadata;
   CREATE TRIGGER delivery_epoch AFTER INSERT OR UPDATE OR DELETE ON archive_metadata FOR EACH ROW EXECUTE FUNCTION delivery_epoch_marker();`)
		return err
	}
	for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
		var body strings.Builder
		for _, row := range []string{"OLD", "NEW"} {
			if (op == "INSERT" && row == "OLD") || (op == "DELETE" && row == "NEW") {
				continue
			}
			body.WriteString(` UPDATE delivery_admission_lock SET generation=generation+1 WHERE singleton=1 AND ` + row + `.key LIKE '` + prefix + `%';
 INSERT INTO delivery_binding_versions(kind,target_id,version,generation) SELECT 'source',CAST(substr(` + row + `.key,` + strconv.Itoa(len(prefix)+1) + `) AS INTEGER),1,(SELECT generation FROM delivery_admission_lock WHERE singleton=1) WHERE ` + row + `.key LIKE '` + prefix + `%' ON CONFLICT(kind,target_id) DO UPDATE SET version=delivery_binding_versions.version+1,generation=excluded.generation;`)
		}
		name := "delivery_marker_" + strings.ToLower(op)
		_, err := s.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+name+`;
 DROP TRIGGER IF EXISTS `+name+`_epoch;
 CREATE TRIGGER `+name+` BEFORE `+op+` ON archive_metadata BEGIN UPDATE delivery_admission_lock SET singleton=singleton WHERE singleton=1; END;
 CREATE TRIGGER `+name+`_epoch AFTER `+op+` ON archive_metadata BEGIN `+body.String()+` END;`)
		if err != nil {
			return err
		}
	}
	return nil
}

// Archive proof can disappear through message retention/deletion independently
// of identity mutation. Fence these writers without treating normal imports as
// identity changes that would revoke every approved mailbox.
func (s *Store) installDeliveryEvidenceFence(ctx context.Context, table string) error {
	if !s.IsPostgreSQL() {
		// Admission updates the singleton row inside its transaction. SQLite's
		// single-writer lock then excludes every other archive write until the
		// provider callback exits, so per-table triggers add work without adding
		// any fencing. In particular, a trigger on messages forces a statement
		// journal for every fresh INSERT even when its body is a no-op.
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DROP TRIGGER IF EXISTS delivery_gate ON `+table+`; CREATE TRIGGER delivery_gate BEFORE INSERT OR UPDATE OR DELETE ON `+table+` FOR EACH STATEMENT EXECUTE FUNCTION delivery_enter_admission();`)
	return err
}

func (s *Store) installDeliveryEmailEvidenceTracking(ctx context.Context) error {
	if s.IsPostgreSQL() {
		if err := s.installPostgresDeliveryEmailEvidenceTracking(ctx); err != nil {
			return err
		}
	} else if err := s.installSQLiteDeliveryEmailEvidenceTracking(boundQuerier{ctx: ctx, q: s.db}); err != nil {
		return err
	}
	const backfill = `INSERT INTO delivery_source_email_evidence(participant_id,source_id,evidence_present,version,generation)
 SELECT pairs.participant_id,pairs.source_id,TRUE,1,(SELECT generation FROM delivery_admission_lock WHERE singleton=1)
 FROM (
   SELECT m.sender_id AS participant_id,m.source_id FROM messages m
   WHERE m.sender_id IS NOT NULL AND m.message_type='email' AND m.deleted_from_source_at IS NULL
   UNION
   SELECT mr.participant_id,m.source_id FROM message_recipients mr JOIN messages m ON m.id=mr.message_id
   WHERE m.message_type='email' AND m.deleted_from_source_at IS NULL
 ) pairs WHERE pairs.participant_id IS NOT NULL
 ON CONFLICT(participant_id,source_id) DO NOTHING`
	if err := s.runOnceMigration(ctx, migrationDeliverySourceEmailEvidenceV1, 1, false, func(ctx context.Context) error {
		return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			if _, err := tx.ExecContext(ctx, backfill); err != nil {
				return fmt.Errorf("backfill participant/source email evidence: %w", err)
			}
			return nil
		})
	}); err != nil {
		return fmt.Errorf("migrate participant/source email evidence: %w", err)
	}
	return nil
}

func (s *Store) installPostgresDeliveryEmailEvidenceTracking(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION delivery_refresh_source_email_evidence(p_participant_id BIGINT,p_source_id BIGINT,p_exclude_message_id BIGINT DEFAULT NULL) RETURNS VOID LANGUAGE plpgsql AS $$
 DECLARE previous_present BOOLEAN; found_pair BOOLEAN; current_present BOOLEAN; current_generation BIGINT;
 BEGIN
   -- A row trigger refreshes the sender and then each recipient. Serialize
   -- refreshes per schema so crossed sender/recipient pairs cannot acquire
   -- participant locks in opposite orders.
   PERFORM pg_advisory_xact_lock(hashtextextended(
     'msgvault.delivery_source_email_evidence:' || current_schema(), 0));
   SELECT evidence_present INTO previous_present FROM delivery_source_email_evidence WHERE participant_id=p_participant_id AND source_id=p_source_id;
   found_pair:=FOUND;
   SELECT EXISTS(
     SELECT 1 FROM messages m WHERE m.sender_id=p_participant_id AND m.source_id=p_source_id AND m.message_type='email' AND m.deleted_from_source_at IS NULL AND (p_exclude_message_id IS NULL OR m.id<>p_exclude_message_id)
   ) OR EXISTS(
     SELECT 1 FROM message_recipients mr JOIN messages m ON m.id=mr.message_id WHERE mr.participant_id=p_participant_id AND m.source_id=p_source_id AND m.message_type='email' AND m.deleted_from_source_at IS NULL AND (p_exclude_message_id IS NULL OR m.id<>p_exclude_message_id)
   ) INTO current_present;
   IF NOT found_pair THEN
     IF NOT current_present THEN RETURN; END IF;
   ELSIF previous_present=current_present THEN
     RETURN;
   END IF;
   UPDATE delivery_admission_lock SET generation=generation+1 WHERE singleton=1 RETURNING generation INTO current_generation;
   INSERT INTO delivery_source_email_evidence(participant_id,source_id,evidence_present,version,generation)
   VALUES (p_participant_id,p_source_id,current_present,1,current_generation)
   ON CONFLICT(participant_id,source_id) DO UPDATE SET evidence_present=excluded.evidence_present,version=delivery_source_email_evidence.version+1,generation=excluded.generation;
 END $$;
 CREATE OR REPLACE FUNCTION delivery_track_email_message() RETURNS trigger LANGUAGE plpgsql AS $$
   DECLARE recipient RECORD;
   BEGIN
     IF TG_OP='DELETE' THEN
       IF OLD.message_type='email' THEN
         IF OLD.sender_id IS NOT NULL THEN PERFORM delivery_refresh_source_email_evidence(OLD.sender_id,OLD.source_id,OLD.id); END IF;
         FOR recipient IN SELECT participant_id FROM message_recipients WHERE message_id=OLD.id LOOP
           PERFORM delivery_refresh_source_email_evidence(recipient.participant_id,OLD.source_id,OLD.id);
         END LOOP;
       END IF;
       RETURN OLD;
     END IF;
     IF TG_OP='UPDATE' AND OLD.message_type='email' THEN
       IF OLD.sender_id IS NOT NULL THEN PERFORM delivery_refresh_source_email_evidence(OLD.sender_id,OLD.source_id); END IF;
       FOR recipient IN SELECT participant_id FROM message_recipients WHERE message_id=OLD.id LOOP
         PERFORM delivery_refresh_source_email_evidence(recipient.participant_id,OLD.source_id);
       END LOOP;
     END IF;
     IF NEW.message_type='email' THEN
       IF NEW.sender_id IS NOT NULL THEN PERFORM delivery_refresh_source_email_evidence(NEW.sender_id,NEW.source_id); END IF;
       FOR recipient IN SELECT participant_id FROM message_recipients WHERE message_id=NEW.id LOOP
         PERFORM delivery_refresh_source_email_evidence(recipient.participant_id,NEW.source_id);
       END LOOP;
     END IF;
     RETURN NEW;
   END $$;
 CREATE OR REPLACE FUNCTION delivery_track_email_recipient() RETURNS trigger LANGUAGE plpgsql AS $$
   DECLARE old_source_id BIGINT; old_message_type TEXT; new_source_id BIGINT; new_message_type TEXT;
   BEGIN
     IF TG_OP<>'INSERT' THEN
       SELECT source_id,message_type INTO old_source_id,old_message_type FROM messages WHERE id=OLD.message_id;
       IF FOUND AND old_message_type='email' THEN PERFORM delivery_refresh_source_email_evidence(OLD.participant_id,old_source_id); END IF;
     END IF;
     IF TG_OP<>'DELETE' THEN
       SELECT source_id,message_type INTO new_source_id,new_message_type FROM messages WHERE id=NEW.message_id;
       IF FOUND AND new_message_type='email' THEN PERFORM delivery_refresh_source_email_evidence(NEW.participant_id,new_source_id); END IF;
     END IF;
     RETURN NULL;
   END $$;
 DROP TRIGGER IF EXISTS delivery_source_email_message_delete ON messages;
 CREATE TRIGGER delivery_source_email_message_delete BEFORE DELETE ON messages FOR EACH ROW EXECUTE FUNCTION delivery_track_email_message();
 DROP TRIGGER IF EXISTS delivery_source_email_message_write ON messages;
 CREATE TRIGGER delivery_source_email_message_write AFTER INSERT OR UPDATE OF source_id, sender_id, message_type, deleted_from_source_at ON messages FOR EACH ROW EXECUTE FUNCTION delivery_track_email_message();
 DROP TRIGGER IF EXISTS delivery_source_email_recipient_write ON message_recipients;
 CREATE TRIGGER delivery_source_email_recipient_write AFTER INSERT OR UPDATE OR DELETE ON message_recipients FOR EACH ROW EXECUTE FUNCTION delivery_track_email_recipient();`)
	return err
}

func deliveryEmailEvidencePresenceSQL(participantID, sourceID, excludeMessageID string) string {
	exclude := ""
	if excludeMessageID != "" {
		exclude = " AND m.id<>" + excludeMessageID
	}
	return `(EXISTS(SELECT 1 FROM messages m WHERE m.sender_id=` + participantID + ` AND m.source_id=` + sourceID + ` AND m.message_type='email' AND m.deleted_from_source_at IS NULL` + exclude + `) OR EXISTS(SELECT 1 FROM message_recipients mr JOIN messages m ON m.id=mr.message_id WHERE mr.participant_id=` + participantID + ` AND m.source_id=` + sourceID + ` AND m.message_type='email' AND m.deleted_from_source_at IS NULL` + exclude + `))`
}

func deliverySQLiteEmailEvidenceTransitionSQL(pairs, excludeMessageID string) string {
	present := deliveryEmailEvidencePresenceSQL("pair.participant_id", "pair.source_id", excludeMessageID)
	state := `SELECT pair.participant_id,pair.source_id,` + present + ` AS evidence_present FROM (` + pairs + `) pair WHERE pair.participant_id IS NOT NULL`
	changed := `COALESCE(existing.evidence_present,0)<>candidate.evidence_present AND (candidate.evidence_present OR existing.participant_id IS NOT NULL)`
	return `UPDATE delivery_admission_lock SET generation=generation+1 WHERE singleton=1 AND EXISTS(
 SELECT 1 FROM (` + state + `) candidate LEFT JOIN delivery_source_email_evidence existing ON existing.participant_id=candidate.participant_id AND existing.source_id=candidate.source_id WHERE ` + changed + `);
 INSERT INTO delivery_source_email_evidence(participant_id,source_id,evidence_present,version,generation)
 SELECT candidate.participant_id,candidate.source_id,candidate.evidence_present,1,(SELECT generation FROM delivery_admission_lock WHERE singleton=1)
 FROM (` + state + `) candidate LEFT JOIN delivery_source_email_evidence existing ON existing.participant_id=candidate.participant_id AND existing.source_id=candidate.source_id
 WHERE ` + changed + `
 ON CONFLICT(participant_id,source_id) DO UPDATE SET evidence_present=excluded.evidence_present,version=delivery_source_email_evidence.version+1,generation=excluded.generation;`
}

func dropSQLiteDeliveryEmailEvidenceTracking(q querier) error {
	for _, name := range []string{
		"delivery_source_email_message_delete",
		"delivery_source_email_message_insert",
		"delivery_source_email_message_update",
		"delivery_source_email_recipient_insert",
		"delivery_source_email_recipient_update",
		"delivery_source_email_recipient_delete",
	} {
		if _, err := q.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
			return fmt.Errorf("drop %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) installSQLiteDeliveryEmailEvidenceTracking(q querier) error {
	const oldMessagePairs = `SELECT OLD.sender_id AS participant_id,OLD.source_id AS source_id WHERE OLD.message_type='email' AND OLD.sender_id IS NOT NULL UNION SELECT mr.participant_id,OLD.source_id FROM message_recipients mr WHERE mr.message_id=OLD.id AND OLD.message_type='email'`
	const newMessagePairs = `SELECT NEW.sender_id AS participant_id,NEW.source_id AS source_id WHERE NEW.message_type='email' AND NEW.sender_id IS NOT NULL UNION SELECT mr.participant_id,NEW.source_id FROM message_recipients mr WHERE mr.message_id=NEW.id AND NEW.message_type='email'`
	const oldRecipientPairs = `SELECT OLD.participant_id AS participant_id,m.source_id FROM messages m WHERE m.id=OLD.message_id AND m.message_type='email'`
	const newRecipientPairs = `SELECT NEW.participant_id AS participant_id,m.source_id FROM messages m WHERE m.id=NEW.message_id AND m.message_type='email'`
	statements := []string{
		`DROP TRIGGER IF EXISTS delivery_source_email_message_delete;
 CREATE TRIGGER delivery_source_email_message_delete BEFORE DELETE ON messages BEGIN ` + deliverySQLiteEmailEvidenceTransitionSQL(oldMessagePairs, "OLD.id") + ` END;`,
		`DROP TRIGGER IF EXISTS delivery_source_email_message_insert;`,
		`DROP TRIGGER IF EXISTS delivery_source_email_message_update;
 CREATE TRIGGER delivery_source_email_message_update AFTER UPDATE OF source_id,sender_id,message_type,deleted_from_source_at ON messages
 WHEN OLD.source_id IS NOT NEW.source_id OR OLD.sender_id IS NOT NEW.sender_id OR OLD.message_type IS NOT NEW.message_type OR OLD.deleted_from_source_at IS NOT NEW.deleted_from_source_at
 BEGIN ` + deliverySQLiteEmailEvidenceTransitionSQL(oldMessagePairs+` UNION `+newMessagePairs, "") + ` END;`,
		`DROP TRIGGER IF EXISTS delivery_source_email_recipient_insert;
 CREATE TRIGGER delivery_source_email_recipient_insert AFTER INSERT ON message_recipients BEGIN ` + deliverySQLiteEmailEvidenceTransitionSQL(newRecipientPairs, "") + ` END;`,
		`DROP TRIGGER IF EXISTS delivery_source_email_recipient_update;
 CREATE TRIGGER delivery_source_email_recipient_update AFTER UPDATE OF message_id,participant_id ON message_recipients
 WHEN OLD.message_id IS NOT NEW.message_id OR OLD.participant_id IS NOT NEW.participant_id
 BEGIN ` + deliverySQLiteEmailEvidenceTransitionSQL(oldRecipientPairs+` UNION `+newRecipientPairs, "") + ` END;`,
		`DROP TRIGGER IF EXISTS delivery_source_email_recipient_delete;
 CREATE TRIGGER delivery_source_email_recipient_delete AFTER DELETE ON message_recipients BEGIN ` + deliverySQLiteEmailEvidenceTransitionSQL(oldRecipientPairs, "") + ` END;`,
	}
	for _, statement := range statements {
		if _, err := q.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func recordSQLiteDeliverySourceEmailEvidence(q querier, participantID, sourceID int64) error {
	result, err := q.Exec(`
		UPDATE delivery_admission_lock SET generation=generation+1
		WHERE singleton=1 AND NOT EXISTS (
			SELECT 1 FROM delivery_source_email_evidence
			WHERE participant_id=? AND source_id=? AND evidence_present=TRUE
		)
	`, participantID, sourceID)
	if err != nil {
		return fmt.Errorf("advance delivery evidence generation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check delivery evidence generation: %w", err)
	}
	if changed == 0 {
		return nil
	}
	if _, err := q.Exec(`
		INSERT INTO delivery_source_email_evidence(participant_id,source_id,evidence_present,version,generation)
		SELECT ?,?,TRUE,1,generation FROM delivery_admission_lock WHERE singleton=1
		ON CONFLICT(participant_id,source_id) DO UPDATE SET
			evidence_present=TRUE,
			version=delivery_source_email_evidence.version+1,
			generation=excluded.generation
		WHERE delivery_source_email_evidence.evidence_present=FALSE
	`, participantID, sourceID); err != nil {
		return fmt.Errorf("record delivery email evidence: %w", err)
	}
	return nil
}
