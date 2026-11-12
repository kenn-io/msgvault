-- Historical trigger emitted by the parent branch before document removal.
CREATE TRIGGER trg_person_sweep_documents_message_update
			    AFTER UPDATE OF sender_id, conversation_id, deleted_at, deleted_from_source_at ON messages FOR EACH ROW WHEN EXISTS (SELECT 1 FROM person_tracking) AND (OLD.sender_id IS NOT NEW.sender_id
		 OR OLD.conversation_id IS NOT NEW.conversation_id
		 OR OLD.deleted_at IS NOT NEW.deleted_at
		 OR OLD.deleted_from_source_at IS NOT NEW.deleted_from_source_at)
			    BEGIN
			        UPDATE person_sweep_change_clock
			           SET sequence = sequence + (SELECT COUNT(*) FROM (SELECT affected.person_id,'document_text' AS source_lane,
			o.source_id,o.message_id,o.attachment_id,o.occurrence_key
			FROM (SELECT * FROM (
			SELECT pp.person_id,
			       CASE WHEN OLD.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       OLD.source_id AS source_id, OLD.id AS message_id
			FROM person_participants pp
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE pp.participant_id = OLD.sender_id
			UNION
			SELECT pp.person_id,
			       CASE WHEN OLD.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       OLD.source_id, OLD.id
			FROM message_recipients mr
			JOIN person_participants pp ON pp.participant_id = mr.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE mr.message_id = OLD.id AND LOWER(mr.recipient_type) IN ('from', 'to', 'cc', 'bcc')
			UNION
			SELECT pp.person_id,
			       CASE WHEN OLD.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       OLD.source_id, OLD.id
			FROM conversations c
			JOIN conversation_participants cp ON cp.conversation_id = c.id
			JOIN person_participants pp ON pp.participant_id = cp.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE c.id = OLD.conversation_id AND (
		(LOWER(COALESCE(c.conversation_type, '')) = 'direct_chat'
		 AND (OLD.is_from_me = TRUE OR OLD.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = OLD.id
		  AND LOWER(known_from.recipient_type) = 'from'))
		 AND NOT EXISTS (
			 SELECT 1 FROM person_participants sender_person
			 WHERE sender_person.person_id = pp.person_id
			   AND (sender_person.participant_id = OLD.sender_id OR (
				 OLD.sender_id IS NULL AND EXISTS (
					 SELECT 1 FROM message_recipients scoped_sender
					 WHERE scoped_sender.message_id = OLD.id
					   AND LOWER(scoped_sender.recipient_type) = 'from'
					   AND scoped_sender.participant_id = sender_person.participant_id)))))
		OR (LOWER(COALESCE(c.conversation_type, '')) <> 'direct_chat' OR NOT (OLD.is_from_me = TRUE OR OLD.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = OLD.id
		  AND LOWER(known_from.recipient_type) = 'from')))))
		 WHERE OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		 UNION
		 SELECT * FROM (
			SELECT pp.person_id,
			       CASE WHEN NEW.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       NEW.source_id AS source_id, NEW.id AS message_id
			FROM person_participants pp
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE pp.participant_id = NEW.sender_id
			UNION
			SELECT pp.person_id,
			       CASE WHEN NEW.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       NEW.source_id, NEW.id
			FROM message_recipients mr
			JOIN person_participants pp ON pp.participant_id = mr.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE mr.message_id = NEW.id AND LOWER(mr.recipient_type) IN ('from', 'to', 'cc', 'bcc')
			UNION
			SELECT pp.person_id,
			       CASE WHEN NEW.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       NEW.source_id, NEW.id
			FROM conversations c
			JOIN conversation_participants cp ON cp.conversation_id = c.id
			JOIN person_participants pp ON pp.participant_id = cp.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE c.id = NEW.conversation_id AND (
		(LOWER(COALESCE(c.conversation_type, '')) = 'direct_chat'
		 AND (NEW.is_from_me = TRUE OR NEW.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = NEW.id
		  AND LOWER(known_from.recipient_type) = 'from'))
		 AND NOT EXISTS (
			 SELECT 1 FROM person_participants sender_person
			 WHERE sender_person.person_id = pp.person_id
			   AND (sender_person.participant_id = NEW.sender_id OR (
				 NEW.sender_id IS NULL AND EXISTS (
					 SELECT 1 FROM message_recipients scoped_sender
					 WHERE scoped_sender.message_id = NEW.id
					   AND LOWER(scoped_sender.recipient_type) = 'from'
					   AND scoped_sender.participant_id = sender_person.participant_id)))))
		OR (LOWER(COALESCE(c.conversation_type, '')) <> 'direct_chat' OR NOT (NEW.is_from_me = TRUE OR NEW.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = NEW.id
		  AND LOWER(known_from.recipient_type) = 'from')))))
		 WHERE NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL) affected
			JOIN document_occurrences o ON o.message_id=affected.message_id) affected)
			         WHERE singleton = TRUE AND enabled = TRUE;
			        INSERT INTO person_sweep_changes
			            (sequence, person_id, source_lane, change_kind, evidence_effect,
			             source_id, message_id, attachment_id, occurrence_key, recorded_at)
			        SELECT c.sequence - totals.total +
			                   ROW_NUMBER() OVER (ORDER BY affected.person_id,
			                                                affected.source_id,
			                                                affected.message_id,
			                                                affected.attachment_id,
			                                                affected.occurrence_key),
			               affected.person_id, affected.source_lane, CASE WHEN (OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL)
		            AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		       THEN 'delete' ELSE 'scope' END, CASE WHEN OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		            AND (NEW.deleted_at IS NOT NULL OR NEW.deleted_from_source_at IS NOT NULL)
		       THEN 'source-deleted'
		       WHEN (OLD.deleted_at IS NOT NULL OR OLD.deleted_from_source_at IS NOT NULL)
		            AND NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL
		       THEN 'source-reimported'
		       ELSE 'identity-reassigned' END,
			               affected.source_id, affected.message_id,
			               NULLIF(affected.attachment_id,0),affected.occurrence_key,CURRENT_TIMESTAMP
			          FROM (SELECT affected.person_id,'document_text' AS source_lane,
			o.source_id,o.message_id,o.attachment_id,o.occurrence_key
			FROM (SELECT * FROM (
			SELECT pp.person_id,
			       CASE WHEN OLD.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       OLD.source_id AS source_id, OLD.id AS message_id
			FROM person_participants pp
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE pp.participant_id = OLD.sender_id
			UNION
			SELECT pp.person_id,
			       CASE WHEN OLD.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       OLD.source_id, OLD.id
			FROM message_recipients mr
			JOIN person_participants pp ON pp.participant_id = mr.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE mr.message_id = OLD.id AND LOWER(mr.recipient_type) IN ('from', 'to', 'cc', 'bcc')
			UNION
			SELECT pp.person_id,
			       CASE WHEN OLD.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       OLD.source_id, OLD.id
			FROM conversations c
			JOIN conversation_participants cp ON cp.conversation_id = c.id
			JOIN person_participants pp ON pp.participant_id = cp.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE c.id = OLD.conversation_id AND (
		(LOWER(COALESCE(c.conversation_type, '')) = 'direct_chat'
		 AND (OLD.is_from_me = TRUE OR OLD.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = OLD.id
		  AND LOWER(known_from.recipient_type) = 'from'))
		 AND NOT EXISTS (
			 SELECT 1 FROM person_participants sender_person
			 WHERE sender_person.person_id = pp.person_id
			   AND (sender_person.participant_id = OLD.sender_id OR (
				 OLD.sender_id IS NULL AND EXISTS (
					 SELECT 1 FROM message_recipients scoped_sender
					 WHERE scoped_sender.message_id = OLD.id
					   AND LOWER(scoped_sender.recipient_type) = 'from'
					   AND scoped_sender.participant_id = sender_person.participant_id)))))
		OR (LOWER(COALESCE(c.conversation_type, '')) <> 'direct_chat' OR NOT (OLD.is_from_me = TRUE OR OLD.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = OLD.id
		  AND LOWER(known_from.recipient_type) = 'from')))))
		 WHERE OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		 UNION
		 SELECT * FROM (
			SELECT pp.person_id,
			       CASE WHEN NEW.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       NEW.source_id AS source_id, NEW.id AS message_id
			FROM person_participants pp
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE pp.participant_id = NEW.sender_id
			UNION
			SELECT pp.person_id,
			       CASE WHEN NEW.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       NEW.source_id, NEW.id
			FROM message_recipients mr
			JOIN person_participants pp ON pp.participant_id = mr.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE mr.message_id = NEW.id AND LOWER(mr.recipient_type) IN ('from', 'to', 'cc', 'bcc')
			UNION
			SELECT pp.person_id,
			       CASE WHEN NEW.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       NEW.source_id, NEW.id
			FROM conversations c
			JOIN conversation_participants cp ON cp.conversation_id = c.id
			JOIN person_participants pp ON pp.participant_id = cp.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE c.id = NEW.conversation_id AND (
		(LOWER(COALESCE(c.conversation_type, '')) = 'direct_chat'
		 AND (NEW.is_from_me = TRUE OR NEW.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = NEW.id
		  AND LOWER(known_from.recipient_type) = 'from'))
		 AND NOT EXISTS (
			 SELECT 1 FROM person_participants sender_person
			 WHERE sender_person.person_id = pp.person_id
			   AND (sender_person.participant_id = NEW.sender_id OR (
				 NEW.sender_id IS NULL AND EXISTS (
					 SELECT 1 FROM message_recipients scoped_sender
					 WHERE scoped_sender.message_id = NEW.id
					   AND LOWER(scoped_sender.recipient_type) = 'from'
					   AND scoped_sender.participant_id = sender_person.participant_id)))))
		OR (LOWER(COALESCE(c.conversation_type, '')) <> 'direct_chat' OR NOT (NEW.is_from_me = TRUE OR NEW.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = NEW.id
		  AND LOWER(known_from.recipient_type) = 'from')))))
		 WHERE NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL) affected
			JOIN document_occurrences o ON o.message_id=affected.message_id) affected
			          CROSS JOIN (SELECT COUNT(*) AS total FROM (SELECT affected.person_id,'document_text' AS source_lane,
			o.source_id,o.message_id,o.attachment_id,o.occurrence_key
			FROM (SELECT * FROM (
			SELECT pp.person_id,
			       CASE WHEN OLD.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       OLD.source_id AS source_id, OLD.id AS message_id
			FROM person_participants pp
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE pp.participant_id = OLD.sender_id
			UNION
			SELECT pp.person_id,
			       CASE WHEN OLD.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       OLD.source_id, OLD.id
			FROM message_recipients mr
			JOIN person_participants pp ON pp.participant_id = mr.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE mr.message_id = OLD.id AND LOWER(mr.recipient_type) IN ('from', 'to', 'cc', 'bcc')
			UNION
			SELECT pp.person_id,
			       CASE WHEN OLD.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       OLD.source_id, OLD.id
			FROM conversations c
			JOIN conversation_participants cp ON cp.conversation_id = c.id
			JOIN person_participants pp ON pp.participant_id = cp.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE c.id = OLD.conversation_id AND (
		(LOWER(COALESCE(c.conversation_type, '')) = 'direct_chat'
		 AND (OLD.is_from_me = TRUE OR OLD.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = OLD.id
		  AND LOWER(known_from.recipient_type) = 'from'))
		 AND NOT EXISTS (
			 SELECT 1 FROM person_participants sender_person
			 WHERE sender_person.person_id = pp.person_id
			   AND (sender_person.participant_id = OLD.sender_id OR (
				 OLD.sender_id IS NULL AND EXISTS (
					 SELECT 1 FROM message_recipients scoped_sender
					 WHERE scoped_sender.message_id = OLD.id
					   AND LOWER(scoped_sender.recipient_type) = 'from'
					   AND scoped_sender.participant_id = sender_person.participant_id)))))
		OR (LOWER(COALESCE(c.conversation_type, '')) <> 'direct_chat' OR NOT (OLD.is_from_me = TRUE OR OLD.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = OLD.id
		  AND LOWER(known_from.recipient_type) = 'from')))))
		 WHERE OLD.deleted_at IS NULL AND OLD.deleted_from_source_at IS NULL
		 UNION
		 SELECT * FROM (
			SELECT pp.person_id,
			       CASE WHEN NEW.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END AS source_lane,
			       NEW.source_id AS source_id, NEW.id AS message_id
			FROM person_participants pp
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE pp.participant_id = NEW.sender_id
			UNION
			SELECT pp.person_id,
			       CASE WHEN NEW.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       NEW.source_id, NEW.id
			FROM message_recipients mr
			JOIN person_participants pp ON pp.participant_id = mr.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE mr.message_id = NEW.id AND LOWER(mr.recipient_type) IN ('from', 'to', 'cc', 'bcc')
			UNION
			SELECT pp.person_id,
			       CASE WHEN NEW.message_type = 'meeting_transcript'
			            THEN 'meeting_text' ELSE 'conversation_text' END,
			       NEW.source_id, NEW.id
			FROM conversations c
			JOIN conversation_participants cp ON cp.conversation_id = c.id
			JOIN person_participants pp ON pp.participant_id = cp.participant_id
			JOIN person_tracking pt ON pt.person_id = pp.person_id
			WHERE c.id = NEW.conversation_id AND (
		(LOWER(COALESCE(c.conversation_type, '')) = 'direct_chat'
		 AND (NEW.is_from_me = TRUE OR NEW.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = NEW.id
		  AND LOWER(known_from.recipient_type) = 'from'))
		 AND NOT EXISTS (
			 SELECT 1 FROM person_participants sender_person
			 WHERE sender_person.person_id = pp.person_id
			   AND (sender_person.participant_id = NEW.sender_id OR (
				 NEW.sender_id IS NULL AND EXISTS (
					 SELECT 1 FROM message_recipients scoped_sender
					 WHERE scoped_sender.message_id = NEW.id
					   AND LOWER(scoped_sender.recipient_type) = 'from'
					   AND scoped_sender.participant_id = sender_person.participant_id)))))
		OR (LOWER(COALESCE(c.conversation_type, '')) <> 'direct_chat' OR NOT (NEW.is_from_me = TRUE OR NEW.sender_id IS NOT NULL OR EXISTS (
		SELECT 1 FROM message_recipients known_from
		WHERE known_from.message_id = NEW.id
		  AND LOWER(known_from.recipient_type) = 'from')))))
		 WHERE NEW.deleted_at IS NULL AND NEW.deleted_from_source_at IS NULL) affected
			JOIN document_occurrences o ON o.message_id=affected.message_id)) totals
			          JOIN person_sweep_change_clock c ON c.singleton = TRUE
			         WHERE c.enabled = TRUE;
			    END;
