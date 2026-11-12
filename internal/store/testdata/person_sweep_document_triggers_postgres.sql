-- Historical trigger definitions from the parent branch, before document removal.
CREATE OR REPLACE FUNCTION msgvault_append_person_sweep_document_changes(
		     _person_id BIGINT, _source_id BIGINT, _message_id BIGINT,
		     _change_kind TEXT, _evidence_effect TEXT)
		 RETURNS VOID AS $$
		 DECLARE occurrence RECORD;
		 DECLARE next_sequence BIGINT;
		 BEGIN
		     FOR occurrence IN
		         SELECT source_id, attachment_id, occurrence_key
		           FROM document_occurrences
		          WHERE message_id = _message_id
		          ORDER BY attachment_id, occurrence_key
		     LOOP
		         next_sequence := NULL;
		         UPDATE person_sweep_change_clock
		            SET sequence = sequence + 1
		          WHERE singleton = TRUE AND enabled = TRUE
		          RETURNING sequence INTO next_sequence;
		         IF next_sequence IS NOT NULL THEN
		             INSERT INTO person_sweep_changes
		                 (sequence, person_id, source_lane, change_kind,
		                  evidence_effect, source_id, message_id, attachment_id,
		                  occurrence_key, recorded_at)
		             VALUES (next_sequence, _person_id, 'document_text', _change_kind,
		                     _evidence_effect, occurrence.source_id, _message_id,
		                     occurrence.attachment_id, occurrence.occurrence_key,
		                     CURRENT_TIMESTAMP);
		         END IF;
		     END LOOP;
		 END;
		 $$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION msgvault_person_sweep_changes_messages() RETURNS trigger AS $$
		 DECLARE affected RECORD;
		 DECLARE kind TEXT;
		 DECLARE effect TEXT;
		 BEGIN
		     IF NOT EXISTS (SELECT 1 FROM person_tracking) THEN
		         IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		     END IF;
		     IF TG_OP = 'INSERT' THEN
		         IF NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL THEN
		             FOR affected IN SELECT * FROM msgvault_person_sweep_message_scope(
		                 NEW.id, NEW.conversation_id, NEW.sender_id, NEW.is_from_me,
		                 NEW.message_type, NEW.source_id)
		             LOOP
		                 PERFORM msgvault_append_person_sweep_change(
		                     affected.person_id, affected.source_lane, 'upsert', '',
		                     affected.source_id, affected.message_id);
		             END LOOP;
		         END IF;
		     ELSIF TG_OP = 'DELETE' THEN
		         IF OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL THEN
		             FOR affected IN SELECT * FROM msgvault_person_sweep_message_scope(
		                 OLD.id, OLD.conversation_id, OLD.sender_id, OLD.is_from_me,
		                 OLD.message_type, OLD.source_id)
		             LOOP
			         PERFORM msgvault_append_person_sweep_change(
			             affected.person_id, affected.source_lane, 'delete', 'source-deleted',
			             affected.source_id, affected.message_id);
			         PERFORM msgvault_append_person_sweep_document_changes(
			             affected.person_id, affected.source_id, affected.message_id,
			             'delete', 'source-deleted');
		             END LOOP;
		         END IF;
		     ELSE
		         kind := CASE
		             WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                  AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		                 THEN 'delete'
		             WHEN OLD.sender_id IS DISTINCT FROM NEW.sender_id
		               OR OLD.conversation_id IS DISTINCT FROM NEW.conversation_id THEN 'scope'
		             ELSE 'upsert' END;
		         effect := CASE
		             WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		                  AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		                 THEN 'source-deleted'
		             WHEN (OLD.deleted_at IS NOT NULL OR OLD.deleted_from_source_at IS NOT NULL)
		                  AND NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		                 THEN 'source-reimported'
		             WHEN OLD.sender_id IS DISTINCT FROM NEW.sender_id
		               OR OLD.conversation_id IS DISTINCT FROM NEW.conversation_id THEN 'identity-reassigned'
		             ELSE 'source-edited' END;
		         FOR affected IN
		             SELECT * FROM msgvault_person_sweep_message_scope(
		                 OLD.id, OLD.conversation_id, OLD.sender_id, OLD.is_from_me,
		                 OLD.message_type, OLD.source_id)
		              WHERE OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		             UNION
		             SELECT * FROM msgvault_person_sweep_message_scope(
		                 NEW.id, NEW.conversation_id, NEW.sender_id, NEW.is_from_me,
		                 NEW.message_type, NEW.source_id)
		              WHERE NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		         LOOP
			     PERFORM msgvault_append_person_sweep_change(
			         affected.person_id, affected.source_lane, kind, effect,
			         affected.source_id, affected.message_id);
			     IF OLD.sender_id IS DISTINCT FROM NEW.sender_id
			        OR OLD.conversation_id IS DISTINCT FROM NEW.conversation_id
			        OR OLD.deleted_at IS DISTINCT FROM NEW.deleted_at
			        OR OLD.deleted_from_source_at IS DISTINCT FROM NEW.deleted_from_source_at THEN
			         PERFORM msgvault_append_person_sweep_document_changes(
			             affected.person_id, affected.source_id, affected.message_id,
			             kind, effect);
			     END IF;
		         END LOOP;
		     END IF;
		     IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
		 END;
		 $$ LANGUAGE plpgsql;
