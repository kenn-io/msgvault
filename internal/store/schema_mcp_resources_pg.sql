-- Install after the legacy source columns have been restored.

CREATE OR REPLACE FUNCTION invalidate_mcp_source_resources() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    DELETE FROM mcp_source_resources WHERE source_id = OLD.id;
    DELETE FROM mcp_conversation_resources WHERE source_id = OLD.id;
    RETURN NEW;
END; $$;
DROP TRIGGER IF EXISTS trg_mcp_source_resources_identity ON sources;
CREATE TRIGGER trg_mcp_source_resources_identity
AFTER UPDATE OF source_type, identifier, google_user_id, oauth_app ON sources
FOR EACH ROW WHEN (OLD.source_type IS DISTINCT FROM NEW.source_type
    OR OLD.identifier IS DISTINCT FROM NEW.identifier
    OR OLD.google_user_id IS DISTINCT FROM NEW.google_user_id
    OR OLD.oauth_app IS DISTINCT FROM NEW.oauth_app)
EXECUTE FUNCTION invalidate_mcp_source_resources();

CREATE OR REPLACE FUNCTION invalidate_mcp_conversation_resources() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    DELETE FROM mcp_conversation_resources WHERE conversation_id = OLD.id;
    RETURN NEW;
END; $$;
DROP TRIGGER IF EXISTS trg_mcp_conversation_resources_identity ON conversations;
CREATE TRIGGER trg_mcp_conversation_resources_identity
AFTER UPDATE OF source_id, source_conversation_id ON conversations
FOR EACH ROW WHEN (OLD.source_id IS DISTINCT FROM NEW.source_id
    OR OLD.source_conversation_id IS DISTINCT FROM NEW.source_conversation_id)
EXECUTE FUNCTION invalidate_mcp_conversation_resources();

CREATE OR REPLACE FUNCTION invalidate_mcp_message_reference() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
    DELETE FROM mcp_event_message_refs WHERE message_id=OLD.id;
    RETURN NEW;
END; $$;
DROP TRIGGER IF EXISTS trg_mcp_message_reference_identity ON messages;
CREATE TRIGGER trg_mcp_message_reference_identity
AFTER UPDATE OF source_id, conversation_id ON messages
FOR EACH ROW WHEN (OLD.source_id IS DISTINCT FROM NEW.source_id
    OR OLD.conversation_id IS DISTINCT FROM NEW.conversation_id)
EXECUTE FUNCTION invalidate_mcp_message_reference();

-- Only account/calendar routing changes rotate calendar source lifetimes.
DROP TRIGGER IF EXISTS trg_mcp_source_resources_calendar_config ON sources;
CREATE TRIGGER trg_mcp_source_resources_calendar_config
AFTER UPDATE OF sync_config ON sources
FOR EACH ROW WHEN (OLD.source_type='gcal' AND (
 (OLD.sync_config->>'account_email') IS DISTINCT FROM (NEW.sync_config->>'account_email')
 OR (OLD.sync_config->>'calendar_id') IS DISTINCT FROM (NEW.sync_config->>'calendar_id')
)) EXECUTE FUNCTION invalidate_mcp_source_resources();
