package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
)

// Old native archives retain thread-parent metadata before native scope rows
// exist. It cannot grant read, but a complete current parent inventory or an
// explicit parent denial must also revoke those retained conversation origins.
// Read metadata and remove origins within the caller's fenced transaction.
func (s *Store) retiredNativeDiscordLegacyOriginsTx(ctx context.Context, tx *sql.Tx, observation NativeMessageScopeInventory, generation string) ([]string, error) {
	if observation.SourceType != sourceTypeDiscord || observation.Kind != "discord_channel" || !observation.Complete && len(observation.retiredOrigins) == 0 {
		return nil, nil
	}
	present := make(map[string]bool, len(observation.Scopes))
	for _, scope := range observation.Scopes {
		present[scope.ProviderID] = true
	}
	denied := make(map[string]bool, len(observation.retiredOrigins))
	for _, parent := range observation.retiredOrigins {
		denied[parent] = true
	}
	// #nosec G701 -- Fixed SQL; exact native source ID and current source lifetime are bound.
	rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT c.source_conversation_id,c.metadata FROM conversations c WHERE c.source_id=? AND c.metadata IS NOT NULL AND EXISTS (SELECT 1 FROM mcp_conversation_resources r WHERE r.conversation_id=c.id AND r.source_id=c.source_id) AND NOT EXISTS (SELECT 1 FROM native_message_scopes n WHERE n.source_id=c.source_id AND n.source_generation=? AND n.scope_kind='discord_thread' AND n.provider_id=c.source_conversation_id)`), observation.SourceID, generation)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var retired []string
	for rows.Next() {
		var providerID, raw string
		if err := rows.Scan(&providerID, &raw); err != nil {
			return nil, err
		}
		var metadata struct {
			GuildID  string `json:"guild_id"`
			ParentID string `json:"parent_channel_id"`
			Kind     *int   `json:"discord_channel_type"`
		}
		if json.Unmarshal([]byte(raw), &metadata) != nil || metadata.GuildID != observation.NativeIdentity || metadata.Kind == nil || !nativeMessageScopeParent(sourceTypeDiscord, "discord_thread", providerID, metadata.ParentID) {
			continue
		}
		// Discord's native announcement, public, and private thread channel types.
		if *metadata.Kind != 10 && *metadata.Kind != 11 && *metadata.Kind != 12 {
			continue
		}
		if denied[metadata.ParentID] || observation.Complete && !present[metadata.ParentID] {
			retired = append(retired, providerID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return retired, rows.Close()
}
