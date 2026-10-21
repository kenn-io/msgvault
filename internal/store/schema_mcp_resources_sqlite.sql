-- Install after the legacy source columns have been restored.

-- Eager invalidation preserves identity changes away and back between reads.
CREATE TRIGGER IF NOT EXISTS trg_mcp_source_resources_identity
AFTER UPDATE OF source_type, identifier, google_user_id, oauth_app ON sources
WHEN OLD.source_type IS NOT NEW.source_type OR OLD.identifier IS NOT NEW.identifier
    OR OLD.google_user_id IS NOT NEW.google_user_id OR OLD.oauth_app IS NOT NEW.oauth_app
BEGIN
    DELETE FROM mcp_source_resources WHERE source_id = OLD.id;
    DELETE FROM mcp_conversation_resources WHERE source_id = OLD.id;
END;
CREATE TRIGGER IF NOT EXISTS trg_mcp_conversation_resources_identity
AFTER UPDATE OF source_id, source_conversation_id ON conversations
WHEN OLD.source_id IS NOT NEW.source_id OR OLD.source_conversation_id IS NOT NEW.source_conversation_id
BEGIN
    DELETE FROM mcp_conversation_resources WHERE conversation_id = OLD.id;
END;
CREATE TRIGGER IF NOT EXISTS trg_mcp_message_reference_identity
AFTER UPDATE OF source_id, conversation_id ON messages
WHEN OLD.source_id IS NOT NEW.source_id OR OLD.conversation_id IS NOT NEW.conversation_id
BEGIN
    DELETE FROM mcp_event_message_refs WHERE message_id=OLD.id;
END;

-- Calendar account routing lives in sync_config independently of its archive
-- identifier. Presentation metadata does not change a resource lifetime.
CREATE TRIGGER IF NOT EXISTS trg_mcp_source_resources_calendar_config
AFTER UPDATE OF sync_config ON sources
WHEN OLD.source_type='gcal' AND (
 json_extract(CASE WHEN json_valid(OLD.sync_config) THEN OLD.sync_config ELSE '{}' END,'$.account_email')
 IS NOT json_extract(CASE WHEN json_valid(NEW.sync_config) THEN NEW.sync_config ELSE '{}' END,'$.account_email')
 OR json_extract(CASE WHEN json_valid(OLD.sync_config) THEN OLD.sync_config ELSE '{}' END,'$.calendar_id')
 IS NOT json_extract(CASE WHEN json_valid(NEW.sync_config) THEN NEW.sync_config ELSE '{}' END,'$.calendar_id')
)
BEGIN
 DELETE FROM mcp_source_resources WHERE source_id=OLD.id;
 DELETE FROM mcp_conversation_resources WHERE source_id=OLD.id;
END;
